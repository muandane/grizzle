package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/plan"
)

func TestMultiSchema_PostgresRoundTrip(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	ts := time.Now().UnixNano()
	s1 := fmt.Sprintf("ms_ident_%d", ts)
	s2 := fmt.Sprintf("ms_bill_%d", ts)

	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s1))
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s2))
	}()

	schemaSQL := fmt.Sprintf(`
		CREATE TABLE %q.users (
			id INT PRIMARY KEY,
			username TEXT NOT NULL
		);

		CREATE TABLE %q.accounts (
			account_id INT PRIMARY KEY,
			user_id INT NOT NULL REFERENCES %q.users(id),
			balance INT NOT NULL DEFAULT 0
		);
	`, s1, s2, s1)

	ctx := context.Background()

	// 1. Initial Sync
	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     schemaSQL,
	})
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Verify live tables and cross-schema FK in catalog
	s1Schema, err := postgres.Inspect(ctx, db, s1)
	if err != nil {
		t.Fatalf("Inspect s1 failed: %v", err)
	}
	if _, ok := s1Schema.Tables["users"]; !ok {
		t.Fatalf("expected table users in %s", s1)
	}

	s2Schema, err := postgres.Inspect(ctx, db, s2)
	if err != nil {
		t.Fatalf("Inspect s2 failed: %v", err)
	}
	tblAccounts, ok := s2Schema.Tables["accounts"]
	if !ok {
		t.Fatalf("expected table accounts in %s", s2)
	}
	if len(tblAccounts.ForeignKeys) == 0 {
		t.Fatalf("expected foreign key on accounts in %s", s2)
	}

	// 2. Round-trip idempotency: PlanDiff must be completely empty
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     schemaSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(p.Steps) != 0 {
		for i, s := range p.Steps {
			t.Logf("unexpected step %d: %s (%s)", i, s.Type, s.SQL)
		}
		t.Fatalf("expected 0 diff steps on round-trip, got %d", len(p.Steps))
	}

	// 3. Check must report nil (no drift)
	if err := grizzle.Check(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     schemaSQL,
	}); err != nil {
		t.Errorf("Check returned unexpected error: %v", err)
	}
}

func TestMultiSchema_CrossSchemaFKToposortAndDrop(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	ts := time.Now().UnixNano()
	s1 := fmt.Sprintf("ms_user_%d", ts)
	s2 := fmt.Sprintf("ms_order_%d", ts)

	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s1))
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s2))
	}()

	schemaSQL := fmt.Sprintf(`
		CREATE TABLE %q.users (
			id INT PRIMARY KEY,
			email TEXT NOT NULL
		);

		CREATE TABLE %q.orders (
			id INT PRIMARY KEY,
			user_id INT NOT NULL REFERENCES %q.users(id)
		);
	`, s1, s2, s1)

	ctx := context.Background()

	// 1. PlanDiff to verify creation topological order: users before orders
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     schemaSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	userCreateIdx, orderCreateIdx := -1, -1
	for i, step := range p.Steps {
		if step.Type == plan.ChangeCreateTable && step.Table == "users" && step.Schema == s1 {
			userCreateIdx = i
		}
		if step.Type == plan.ChangeCreateTable && step.Table == "orders" && step.Schema == s2 {
			orderCreateIdx = i
		}
	}
	if userCreateIdx == -1 || orderCreateIdx == -1 {
		t.Fatalf("expected ChangeCreateTable for both users and orders, got userIdx=%d, orderIdx=%d", userCreateIdx, orderCreateIdx)
	}
	if userCreateIdx > orderCreateIdx {
		t.Fatalf("expected users (referenced) created before orders (referencing), got userIdx=%d > orderIdx=%d", userCreateIdx, orderCreateIdx)
	}

	// 2. Apply the plan using grizzle.Apply with expected hash verification
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{
		ExpectedHash: p.Hash(),
	})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// 3. Plan drop: empty SchemaSQL with AllowDrop: true
	dropPlan, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     fmt.Sprintf("CREATE TABLE %q._dummy (id INT); CREATE TABLE %q._dummy2 (id INT);", s1, s2),
		AllowDrop:     true,
	})
	if err != nil {
		t.Fatalf("PlanDiff for drop failed: %v", err)
	}

	userDropIdx, orderDropIdx := -1, -1
	for i, step := range dropPlan.Steps {
		if step.Type == plan.ChangeDropTable && step.Table == "users" && step.Schema == s1 {
			userDropIdx = i
		}
		if step.Type == plan.ChangeDropTable && step.Table == "orders" && step.Schema == s2 {
			orderDropIdx = i
		}
	}

	if userDropIdx != -1 && orderDropIdx != -1 {
		if orderDropIdx > userDropIdx {
			t.Fatalf("expected orders (referencing) dropped before users (referenced), got orderDropIdx=%d > userDropIdx=%d", orderDropIdx, userDropIdx)
		}
	}
}

func TestMultiSchema_AdvisoryLockKeyGeneration(t *testing.T) {
	// 1. Single schema compatibility
	singleLockID := grizzle.GenerateLockID("grizzle", "public")
	if singleLockID == 0 {
		t.Errorf("expected non-zero lock ID")
	}

	// 2. Multi-schema combination: sorted schemas
	lock1 := grizzle.GenerateLockID("grizzle", "billing,identity,public")
	lock2 := grizzle.GenerateLockID("grizzle", "billing,identity,public")
	if lock1 != lock2 {
		t.Errorf("expected deterministic lock hash")
	}

	// Distinct schema sets must produce distinct lock IDs
	lockDistinct := grizzle.GenerateLockID("grizzle", "analytics,public")
	if lock1 == lockDistinct {
		t.Errorf("expected distinct lock IDs for distinct schema sets")
	}

	// Permuted schema slice order should yield same lock ID through Options
	opts1 := grizzle.Options{
		TargetSchemas: []string{"public", "identity", "billing"},
		SchemaSQL:     "CREATE TABLE public.t (id INT);",
	}
	opts2 := grizzle.Options{
		TargetSchemas: []string{"billing", "public", "identity"},
		SchemaSQL:     "CREATE TABLE public.t (id INT);",
	}
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	p1, err := grizzle.PlanDiff(context.Background(), db, opts1)
	if err != nil {
		t.Fatalf("PlanDiff opts1 failed: %v", err)
	}
	p2, err := grizzle.PlanDiff(context.Background(), db, opts2)
	if err != nil {
		t.Fatalf("PlanDiff opts2 failed: %v", err)
	}
	if p1.Hash() != p2.Hash() {
		t.Errorf("expected plan hash to be invariant to input TargetSchemas order: %s vs %s", p1.Hash(), p2.Hash())
	}
}

func TestMultiSchema_ConcurrentOverlappingSync_NoDeadlock(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	ts := time.Now().UnixNano()
	sA := fmt.Sprintf("ms_deadlock_a_%d", ts)
	sB := fmt.Sprintf("ms_deadlock_b_%d", ts)

	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", sA))
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", sB))
	}()

	schemaSQL := fmt.Sprintf(`
		CREATE TABLE %q.t1 (id INT PRIMARY KEY);
		CREATE TABLE %q.t2 (id INT PRIMARY KEY);
	`, sA, sB)

	// App 1 specifies {sA, sB}, App 2 specifies {sB, sA} (reversed)
	opts1 := grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{sA, sB},
		SchemaSQL:     schemaSQL,
	}
	opts2 := grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{sB, sA},
		SchemaSQL:     schemaSQL,
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	startBarrier := make(chan struct{})

	runSync := func(opts grizzle.Options) {
		defer wg.Done()
		<-startBarrier
		if err := grizzle.Sync(context.Background(), db, opts); err != nil {
			errs <- err
		}
	}

	wg.Add(2)
	go runSync(opts1)
	go runSync(opts2)

	close(startBarrier)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent sync with reversed schema sets failed: %v", err)
	}
}

func TestMultiSchema_LockNamespaceIsolation(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	conn1, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed acquiring conn1: %v", err)
	}
	defer func() { _ = conn1.Close() }()

	conn2, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed acquiring conn2: %v", err)
	}
	defer func() { _ = conn2.Close() }()

	schema := "shared_schema_test"
	ns1 := "app_one"
	ns2 := "app_two"

	key1A := postgres.Hash32(ns1)
	key1B := postgres.Hash32(schema)

	key2A := postgres.Hash32(ns2)
	key2B := postgres.Hash32(schema)

	// Acquire lock with namespace ns1 on conn1
	if err := postgres.AcquireSessionAdvisoryLock2(ctx, conn1, key1A, key1B); err != nil {
		t.Fatalf("failed acquiring lock on conn1: %v", err)
	}
	defer func() { _ = postgres.ReleaseSessionAdvisoryLock2(ctx, conn1, key1A, key1B) }()

	// Conn2 should be able to acquire lock immediately because namespace differs
	lockCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	if err := postgres.AcquireSessionAdvisoryLock2(lockCtx, conn2, key2A, key2B); err != nil {
		t.Fatalf("distinct LockNamespace should not block on same schema: %v", err)
	}
	_ = postgres.ReleaseSessionAdvisoryLock2(ctx, conn2, key2A, key2B)
}

func TestMultiSchema_CircularCrossSchemaFKs(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	ts := time.Now().UnixNano()
	s1 := fmt.Sprintf("ms_circ_1_%d", ts)
	s2 := fmt.Sprintf("ms_circ_2_%d", ts)

	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s1))
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s2))
	}()

	schemaSQL := fmt.Sprintf(`
		CREATE TABLE %q.table_a (
			id INT PRIMARY KEY,
			b_id INT
		);

		CREATE TABLE %q.table_b (
			id INT PRIMARY KEY,
			a_id INT,
			CONSTRAINT fk_b_a FOREIGN KEY (a_id) REFERENCES %q.table_a(id)
		);

		CREATE INDEX idx_a_b ON %q.table_a(b_id);
	`, s1, s2, s1, s1)

	ctx := context.Background()

	// 1. Initial Sync creates tables and cross-schema FK
	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     schemaSQL,
	})
	if err != nil {
		t.Fatalf("initial Sync failed: %v", err)
	}

	// 2. Add circular FK from table_a -> table_b
	circularSQL := fmt.Sprintf(`
		CREATE TABLE %q.table_a (
			id INT PRIMARY KEY,
			b_id INT
		);

		CREATE TABLE %q.table_b (
			id INT PRIMARY KEY,
			a_id INT,
			CONSTRAINT fk_b_a FOREIGN KEY (a_id) REFERENCES %q.table_a(id)
		);

		ALTER TABLE %q.table_a ADD CONSTRAINT fk_a_b FOREIGN KEY (b_id) REFERENCES %q.table_b(id);

		CREATE INDEX idx_a_b ON %q.table_a(b_id);
	`, s1, s2, s1, s1, s2, s1)

	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     circularSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff for circular FK failed: %v", err)
	}

	// Verify plan steps contain ADD_FK and VALIDATE_CONSTRAINT
	var hasAddFK, hasValidate bool
	for _, s := range p.Steps {
		if s.Type == plan.ChangeAddFK {
			hasAddFK = true
			if !strings.Contains(s.SQL, "NOT VALID") {
				t.Errorf("expected NOT VALID in ADD_FK, got: %s", s.SQL)
			}
		}
		if s.Type == plan.ChangeValidateConstraint {
			hasValidate = true
		}
	}
	if !hasAddFK || !hasValidate {
		t.Fatalf("expected ADD_FK and VALIDATE_CONSTRAINT steps, got: %+v", p.Steps)
	}

	// Apply circular FK migration
	if err := grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{ExpectedHash: p.Hash()}); err != nil {
		t.Fatalf("Apply circular FK failed: %v", err)
	}

	// Verify plan hash is deterministic
	pAgain, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     circularSQL,
	})
	if err != nil {
		t.Fatalf("recomputed PlanDiff failed: %v", err)
	}
	if len(pAgain.Steps) != 0 {
		t.Fatalf("expected 0 diff steps after applying circular FK, got %d", len(pAgain.Steps))
	}

	// 3. Drop verification: with AllowDrop: true, dropping both tables must drop FKs before dropping tables
	dropSQL := fmt.Sprintf(`CREATE TABLE %q.empty_placeholder (id INT);`, s1)
	dropPlan, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     dropSQL,
		AllowDrop:     true,
	})
	if err != nil {
		t.Fatalf("drop PlanDiff failed: %v", err)
	}

	var firstTableDropIdx = -1
	var lastFKDropIdx = -1
	for i, s := range dropPlan.Steps {
		if s.Type == plan.ChangeDropFK {
			lastFKDropIdx = i
		}
		if s.Type == plan.ChangeDropTable && firstTableDropIdx == -1 {
			firstTableDropIdx = i
		}
	}

	if lastFKDropIdx == -1 {
		t.Fatalf("expected at least one DROP_FK step in drop plan, found none: %+v", dropPlan.Steps)
	}
	if firstTableDropIdx == -1 {
		t.Fatalf("expected at least one DROP_TABLE step in drop plan, found none: %+v", dropPlan.Steps)
	}
	if lastFKDropIdx > firstTableDropIdx {
		t.Errorf("expected all DROP_FK steps before DROP_TABLE steps: lastFK=%d, firstTable=%d", lastFKDropIdx, firstTableDropIdx)
	}

	// 4. Golden plan hash: verify deterministic plan hash for circular cross-schema FK drops with static schemas
	const staticS1 = "circ_static_a"
	const staticS2 = "circ_static_b"
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %s, %s CASCADE;", staticS1, staticS2))
	}()
	_, _ = db.Exec(fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s;", staticS1))
	_, _ = db.Exec(fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s;", staticS2))

	staticCircularSQL := fmt.Sprintf(`
		CREATE TABLE %s.table_a (id INT PRIMARY KEY, b_id INT);
		CREATE TABLE %s.table_b (id INT PRIMARY KEY, a_id INT, CONSTRAINT fk_b_a FOREIGN KEY (a_id) REFERENCES %s.table_a(id));
		ALTER TABLE %s.table_a ADD CONSTRAINT fk_a_b FOREIGN KEY (b_id) REFERENCES %s.table_b(id);
	`, staticS1, staticS2, staticS1, staticS1, staticS2)

	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{staticS1, staticS2},
		SchemaSQL:     staticCircularSQL,
	}); err != nil {
		t.Fatalf("Sync static circular schemas failed: %v", err)
	}

	staticDropSQL := fmt.Sprintf(`CREATE TABLE %s.empty_placeholder (id INT);`, staticS1)
	staticDropPlan, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{staticS1, staticS2},
		SchemaSQL:     staticDropSQL,
		AllowDrop:     true,
	})
	if err != nil {
		t.Fatalf("PlanDiff static drop failed: %v", err)
	}
	const goldenCircularDropHash = "a1d6725512d17779358410cd6fb5ba22fea3a9bc3b9b20e079163b00b529f767"
	if staticDropPlan.Hash() != goldenCircularDropHash {
		t.Errorf("circular FK drop plan golden hash mismatch:\ngot:  %s\nwant: %s", staticDropPlan.Hash(), goldenCircularDropHash)
	}
}

func TestMultiSchema_SQLiteTypedError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:       grizzle.DialectSQLite,
		TargetSchemas: []string{"main", "aux"},
		SchemaSQL:     "CREATE TABLE main.items (id INT);",
	})
	if err == nil {
		t.Fatalf("expected error configuring multiple schemas on SQLite, got nil")
	}
	if !errors.Is(err, grizzle.ErrUnsupportedMultiSchema) {
		t.Fatalf("expected ErrUnsupportedMultiSchema, got: %v", err)
	}
}
