package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestLocking_StepGrouping(t *testing.T) {
	steps := []plan.Step{
		{SQL: "CREATE TABLE t1 (id int);", NonTx: false},
		{SQL: "CREATE TABLE t2 (id int);", NonTx: false},
		{SQL: "CREATE INDEX CONCURRENTLY idx1 ON t1(id);", NonTx: true},
		{SQL: "CREATE INDEX CONCURRENTLY idx2 ON t2(id);", NonTx: true},
		{SQL: "ALTER TABLE t1 VALIDATE CONSTRAINT fk1;", NonTx: false},
	}

	groups := exec.GroupSteps(steps)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}

	if groups[0].NonTx || len(groups[0].Steps) != 2 {
		t.Errorf("group 0 expected 2 tx steps, got NonTx=%t len=%d", groups[0].NonTx, len(groups[0].Steps))
	}
	if !groups[1].NonTx || len(groups[1].Steps) != 2 {
		t.Errorf("group 1 expected 2 non-tx steps, got NonTx=%t len=%d", groups[1].NonTx, len(groups[1].Steps))
	}
	if groups[2].NonTx || len(groups[2].Steps) != 1 {
		t.Errorf("group 2 expected 1 tx step, got NonTx=%t len=%d", groups[2].NonTx, len(groups[2].Steps))
	}
}

func TestLocking_ForeignKeyNotValidAndValidate(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_fk_nv_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initialSQL := `
		CREATE TABLE orgs (id BIGINT PRIMARY KEY);
		CREATE TABLE users (id BIGINT PRIMARY KEY, org_id BIGINT);
	`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Add foreign key
	desiredSQL := `
		CREATE TABLE orgs (id BIGINT PRIMARY KEY);
		CREATE TABLE users (
			id BIGINT PRIMARY KEY,
			org_id BIGINT,
			CONSTRAINT fk_users_org FOREIGN KEY (org_id) REFERENCES orgs(id)
		);
	`

	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}

	var hasNotValid, hasValidate bool
	for _, s := range p.Steps {
		if strings.Contains(s.SQL, "NOT VALID") {
			hasNotValid = true
		}
		if s.Type == grizzle.ChangeValidateConstraint {
			hasValidate = true
		}
	}

	if !hasNotValid {
		t.Errorf("expected step with NOT VALID, got steps: %+v", p.Steps)
	}
	if !hasValidate {
		t.Errorf("expected step with ChangeValidateConstraint, got steps: %+v", p.Steps)
	}

	// Apply migration
	err = grizzle.Apply(context.Background(), db, p, grizzle.ApplyOpts{
		ExpectedHash: p.Hash(),
	})
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
}

func TestLocking_RecomputeDiffPostLock(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_recompute_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initialSQL := `CREATE TABLE items (id BIGINT PRIMARY KEY);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	desiredSQL := `
		CREATE TABLE items (id BIGINT PRIMARY KEY, name TEXT);
	`

	// 1. Generate plan before concurrent modification
	preLockPlan, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	preLockHash := preLockPlan.Hash()

	// 2. Interleave a concurrent modification: someone manually adds a column or table
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s.items ADD COLUMN concurrent_col TEXT;`, schema))
	if err != nil {
		t.Fatalf("failed concurrent alter: %v", err)
	}

	// 3. Applying with preLockHash should abort with ErrPlanDrift because post-lock diff differs!
	err = grizzle.Apply(context.Background(), db, preLockPlan, grizzle.ApplyOpts{
		ExpectedHash: preLockHash,
	})
	if err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("expected ErrPlanDrift on stale pre-lock plan, got: %v", err)
	}
}

func TestLocking_NonConcurrentIndexes(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_concurrent_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initialSQL := `CREATE TABLE docs (id BIGINT PRIMARY KEY, title TEXT);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	desiredSQL := `
		CREATE TABLE docs (id BIGINT PRIMARY KEY, title TEXT);
		CREATE INDEX idx_docs_title ON docs(title);
	`

	// 1. By default, CONCURRENTLY is emitted
	planDefault, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("default plan failed: %v", err)
	}
	if len(planDefault.Steps) != 1 || !planDefault.Steps[0].NonTx || !strings.Contains(planDefault.Steps[0].SQL, "CONCURRENTLY") {
		t.Errorf("expected CONCURRENTLY step with NonTx=true, got: %+v", planDefault.Steps)
	}

	// 2. With NonConcurrentIndexes: true, CONCURRENTLY is omitted
	planNonConcurrent, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:              grizzle.DialectPostgres,
		TargetSchema:         schema,
		SchemaSQL:            desiredSQL,
		NonConcurrentIndexes: true,
	})
	if err != nil {
		t.Fatalf("non-concurrent plan failed: %v", err)
	}
	if len(planNonConcurrent.Steps) != 1 || planNonConcurrent.Steps[0].NonTx || strings.Contains(planNonConcurrent.Steps[0].SQL, "CONCURRENTLY") {
		t.Errorf("expected standard index step without CONCURRENTLY and NonTx=false, got: %+v", planNonConcurrent.Steps)
	}
}

// TestLocking_PreambleDoesNotConsumeLockBudget guards the lock-budget
// accounting: only lock waiting (acquisition + backoff) consumes the
// LockTimeout budget. A slow BeforeSync hook must not shrink the acquisition
// window of its own attempt — the timer is armed when acquisition begins,
// not when the wrapper starts. On the previous implementation the deadline
// was anchored at wrapper start, so this hook delay zeroed the acquisition
// window and Sync failed even though the holder released well inside the
// budget.
func TestLocking_PreambleDoesNotConsumeLockBudget(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_preamble_budget_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	nsKey := postgres.Hash32("grizzle")
	schemaKey := postgres.Hash32(schema)

	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() { _ = holderConn.Close() }()

	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1, $2);", nsKey, schemaKey).Scan(&dummy); err != nil {
		t.Fatalf("holder failed acquiring pg_advisory_lock: %v", err)
	}
	go func() {
		time.Sleep(1200 * time.Millisecond)
		var released bool
		_ = holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1, $2);", nsKey, schemaKey).Scan(&released)
	}()

	// Slow preamble: 500ms. Budget: 1s. Holder releases at 1.2s. With the
	// acquisition timer armed when acquisition begins, the window is
	// [500ms, 1.5s] and covers the release. Anchoring the deadline at wrapper
	// start instead makes the window [500ms, 1s] — it expires before the
	// holder releases, and the backoff reserve then aborts the campaign.
	hookRan := make(chan struct{}, 1)
	start := time.Now()
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    "CREATE TABLE preamble_probe (id BIGINT PRIMARY KEY);",
		LockTimeout:  1 * time.Second,
		MaxRetries:   2,
		BeforeSync: func(ctx context.Context, dbtx dialect.DBTX) error {
			select {
			case hookRan <- struct{}{}:
			default:
			}
			time.Sleep(500 * time.Millisecond)
			return nil
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("sync failed: slow preamble must not consume the acquisition budget: %v", err)
	}
	select {
	case <-hookRan:
	default:
		t.Errorf("BeforeSync hook did not run")
	}
	if elapsed < 700*time.Millisecond {
		t.Errorf("sync finished before the holder released: %v", elapsed)
	}
}
func TestLocking_DedicatedSessionAdvisoryLock_ContentionRetry(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_adv_retry_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	nsKey := postgres.Hash32("grizzle")
	schemaKey := postgres.Hash32(schema)

	// 1. Holder connection grabs the dedicated session advisory lock directly
	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() { _ = holderConn.Close() }()

	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1, $2);", nsKey, schemaKey).Scan(&dummy); err != nil {
		t.Fatalf("holder failed acquiring pg_advisory_lock: %v", err)
	}

	// 2. Release holder lock after 150ms
	go func() {
		time.Sleep(150 * time.Millisecond)
		var released bool
		_ = holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1, $2);", nsKey, schemaKey).Scan(&released)
	}()

	// 3. Migration attempts Sync with a 1s TOTAL lock budget and MaxRetries=5.
	// LockTimeout bounds the entire acquisition across retries (Task: total
	// lock budget), while MaxRetries still applies to retryable execution
	// errors (deadlock, 55P03). Holder releases at 150ms, well inside budget.
	desiredSQL := `CREATE TABLE products (id BIGINT PRIMARY KEY, name TEXT);`
	start := time.Now()
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:          grizzle.DialectPostgres,
		TargetSchema:     schema,
		SchemaSQL:        desiredSQL,
		LockTimeout:      1 * time.Second,
		StatementTimeout: 5 * time.Second,
		MaxRetries:       5,
	})
	if err != nil {
		t.Fatalf("migration failed despite retries on advisory lock contention: %v", err)
	}

	if time.Since(start) < 100*time.Millisecond {
		t.Errorf("migration should have waited for advisory lock release (expected >= 100ms, took %v)", time.Since(start))
	}
}

func TestLocking_DedicatedSessionAdvisoryLock_ExhaustRetriesFails(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_adv_fail_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	nsKey := postgres.Hash32("grizzle")
	schemaKey := postgres.Hash32(schema)

	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() {
		var released bool
		_ = holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1, $2);", nsKey, schemaKey).Scan(&released)
		_ = holderConn.Close()
	}()

	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1, $2);", nsKey, schemaKey).Scan(&dummy); err != nil {
		t.Fatalf("holder failed acquiring pg_advisory_lock: %v", err)
	}

	// Migration attempts Sync with a 40ms lock timeout and MaxRetries=2; must fail
	desiredSQL := `CREATE TABLE gadgets (id BIGINT PRIMARY KEY);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:          grizzle.DialectPostgres,
		TargetSchema:     schema,
		SchemaSQL:        desiredSQL,
		LockTimeout:      40 * time.Millisecond,
		StatementTimeout: 2 * time.Second,
		MaxRetries:       2,
	})
	if err == nil {
		t.Fatalf("expected migration to fail due to advisory lock contention timeout, got nil")
	}

	if !exec.IsRetryable(err) && !strings.Contains(err.Error(), "lock") {
		t.Errorf("expected lock timeout error, got: %v", err)
	}
}

func TestLocking_DirectApply_DedicatedSessionLockAndNonTx(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_direct_apply_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	desiredSQL := `
		CREATE TABLE books (id BIGINT PRIMARY KEY, title TEXT);
		CREATE INDEX idx_books_title ON books(title);
	`

	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Verify plan contains both Tx and NonTx steps
	var hasNonTx bool
	for _, s := range p.Steps {
		if s.NonTx {
			hasNonTx = true
		}
	}
	if !hasNonTx {
		t.Fatalf("expected plan to have at least one NonTx step (CONCURRENTLY index)")
	}

	// Strip SchemaSQL to force direct Apply fallback execution
	p.SchemaSQL = ""

	err = grizzle.Apply(context.Background(), db, p, grizzle.ApplyOpts{
		ExpectedHash: p.Hash(),
	})
	if err != nil {
		t.Fatalf("direct Apply failed: %v", err)
	}

	// Verify idempotency: re-diffing against desired SQL yields zero steps
	pAfter, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("post-apply PlanDiff failed: %v", err)
	}
	if len(pAfter.Steps) != 0 {
		t.Fatalf("expected 0 steps after direct apply, got %d: %+v", len(pAfter.Steps), pAfter.Steps)
	}
}

func TestLocking_DirectApply_ConcurrentPodMutualExclusion(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_direct_conc_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	desiredSQL := `
		CREATE TABLE items (id BIGINT PRIMARY KEY, name TEXT NOT NULL);
		CREATE INDEX idx_items_name ON items(name);
	`

	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Strip SchemaSQL to test direct Apply path under concurrency
	p.SchemaSQL = ""
	approvedHash := p.Hash()

	const numPods = 5
	var wg sync.WaitGroup
	errs := make(chan error, numPods)
	barrier := make(chan struct{})

	for range numPods {
		wg.Go(func() {
			<-barrier

			planCopy := *p
			if err := grizzle.Apply(context.Background(), db, &planCopy, grizzle.ApplyOpts{
				ExpectedHash: approvedHash,
			}); err != nil {
				errs <- err
			}
		})
	}

	close(barrier)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent direct Apply error: %v", err)
	}

	// Verify idempotency
	pAfter, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff after concurrent apply failed: %v", err)
	}
	if len(pAfter.Steps) != 0 {
		t.Errorf("expected 0 diff steps after concurrent apply, got %d: %+v", len(pAfter.Steps), pAfter.Steps)
	}
}

func TestLocking_DeclarativeSync_ConcurrentPodMutualExclusion(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_decl_conc_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	desiredSQL := `
		CREATE TABLE accounts (id BIGINT PRIMARY KEY, email TEXT NOT NULL);
		CREATE INDEX idx_accounts_email ON accounts(email);
	`

	const numPods = 5
	var wg sync.WaitGroup
	errs := make(chan error, numPods)
	barrier := make(chan struct{})

	for range numPods {
		wg.Go(func() {
			<-barrier

			if err := grizzle.Sync(context.Background(), db, grizzle.Options{
				Dialect:      grizzle.DialectPostgres,
				TargetSchema: schema,
				SchemaSQL:    desiredSQL,
			}); err != nil {
				errs <- err
			}
		})
	}

	close(barrier)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent declarative Apply error: %v", err)
	}

	// Verify idempotency
	pAfter, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff after concurrent apply failed: %v", err)
	}
	if len(pAfter.Steps) != 0 {
		t.Errorf("expected 0 diff steps after concurrent apply, got %d: %+v", len(pAfter.Steps), pAfter.Steps)
	}
}

func TestLocking_PgTerminateBackendRecovery(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_term_rec_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	// 1. Initial table with 50,000 rows
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	if _, err := db.Exec("SELECT set_config('search_path', $1, false);", schemaIdent); err != nil {
		t.Fatalf("failed setting search_path: %v", err)
	}
	const initialSQL = `
		CREATE TABLE large_table (
			id BIGINT PRIMARY KEY,
			email TEXT NOT NULL
		);
		INSERT INTO large_table (id, email)
		SELECT g, 'user_' || g || '@example.com' FROM generate_series(1, 120000) g;
	`
	if _, err := db.Exec(initialSQL); err != nil {
		t.Fatalf("failed setting up initial data: %v", err)
	}

	desiredSQL := `
		CREATE TABLE large_table (
			id BIGINT PRIMARY KEY,
			email TEXT NOT NULL
		);
		CREATE INDEX idx_term_email ON large_table(email);
	`

	// Watcher goroutine to terminate backend mid-CREATE INDEX CONCURRENTLY
	terminateCtx, cancelTerminate := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelTerminate()

	terminated := make(chan bool, 1)
	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-terminateCtx.Done():
				return
			case <-ticker.C:
				var pid int
				query := `
					SELECT pid FROM pg_stat_activity
					WHERE state = 'active'
					  AND query LIKE '%CREATE INDEX CONCURRENTLY%'
					  AND pid <> pg_backend_pid()
					LIMIT 1;
				`
				if err := db.QueryRow(query).Scan(&pid); err == nil && pid > 0 {
					time.Sleep(20 * time.Millisecond)
					var success bool
					_ = db.QueryRow("SELECT pg_terminate_backend($1);", pid).Scan(&success)
					terminated <- true
					return
				}
			}
		}
	}()

	// 2. Sync attempt should fail because the backend is terminated
	syncErr := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if syncErr == nil {
		t.Fatalf("expected error from terminated backend, but sync succeeded")
	}
	t.Logf("syncErr was: %v", syncErr)

	// 3. Verify advisory lock is freed
	testConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring test conn: %v", err)
	}
	defer func() { _ = testConn.Close() }()

	key1 := postgres.Hash32("grizzle")
	key2 := postgres.Hash32(schema)
	lockCtx, cancelLock := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelLock()
	if err := postgres.AcquireSessionAdvisoryLock2(lockCtx, testConn, key1, key2); err != nil {
		t.Fatalf("advisory lock should be freed after backend termination, but failed: %v", err)
	}
	_ = postgres.ReleaseSessionAdvisoryLock2(context.Background(), testConn, key1, key2)

	// 4. Verify invalid index was left behind in pg_index
	var isInvalid bool
	const checkQuery = `
		SELECT NOT i.indisvalid
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2;
	`
	err = db.QueryRow(checkQuery, schema, "idx_term_email").Scan(&isInvalid)
	if err != nil {
		t.Fatalf("querying invalid index failed: %v", err)
	}
	if !isInvalid {
		t.Fatalf("expected index to be marked invalid (indisvalid=false) after interrupted creation")
	}

	// 5. Rerun Sync: auto-recovery should drop invalid index and recreate it cleanly
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("rerun Sync after invalid index failed: %v", err)
	}

	// 6. Final verification: index is now valid and PlanDiff is empty
	var isValid bool
	const validQuery = `
		SELECT i.indisvalid
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2;
	`
	err = db.QueryRow(validQuery, schema, "idx_term_email").Scan(&isValid)
	if err != nil || !isValid {
		t.Fatalf("expected recovered index to be valid, err=%v, isValid=%t", err, isValid)
	}

	pFinal, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("final PlanDiff failed: %v", err)
	}
	if len(pFinal.Steps) != 0 {
		t.Fatalf("expected 0 diff steps after recovery, got %d: %+v", len(pFinal.Steps), pFinal.Steps)
	}
}

// TestLocking_TotalLockWait_BoundedAcrossRetries verifies the advisory-lock
// acquisition budget is TOTAL across retries: Sync must fail after roughly
// LockTimeout, not LockTimeout x (MaxRetries+1).
func TestLocking_TotalLockWait_BoundedAcrossRetries(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_total_budget_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	nsKey := postgres.Hash32("grizzle")
	schemaKey := postgres.Hash32(schema)

	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() {
		var released bool
		_ = holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1, $2);", nsKey, schemaKey).Scan(&released)
		_ = holderConn.Close()
	}()

	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1, $2);", nsKey, schemaKey).Scan(&dummy); err != nil {
		t.Fatalf("holder failed acquiring pg_advisory_lock: %v", err)
	}

	start := time.Now()
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:          grizzle.DialectPostgres,
		TargetSchema:     schema,
		SchemaSQL:        "CREATE TABLE widgets (id BIGINT PRIMARY KEY);",
		LockTimeout:      300 * time.Millisecond,
		MaxRetries:       5,
		StatementTimeout: 5 * time.Second,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected lock acquisition failure, got nil")
	}
	if elapsed >= 2000*time.Millisecond {
		t.Errorf("total lock wait not bounded across retries: took %v (want ~300ms budget + backoff)", elapsed)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("returned before the lock budget could elapse: %v", elapsed)
	}
}

// TestLocking_LockSucceedsWhenReleasedWithinBudget verifies the shared
// deadline does not break the success path: when the holder releases the lock
// inside the budget, Sync succeeds via the polling acquisition.
func TestLocking_LockSucceedsWhenReleasedWithinBudget(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_budget_success_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	nsKey := postgres.Hash32("grizzle")
	schemaKey := postgres.Hash32(schema)

	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() { _ = holderConn.Close() }()

	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1, $2);", nsKey, schemaKey).Scan(&dummy); err != nil {
		t.Fatalf("holder failed acquiring pg_advisory_lock: %v", err)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		var released bool
		_ = holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1, $2);", nsKey, schemaKey).Scan(&released)
	}()

	start := time.Now()
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:          grizzle.DialectPostgres,
		TargetSchema:     schema,
		SchemaSQL:        "CREATE TABLE widgets (id BIGINT PRIMARY KEY);",
		LockTimeout:      1 * time.Second,
		MaxRetries:       3,
		StatementTimeout: 5 * time.Second,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected sync to succeed after holder released within budget, got: %v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("sync did not wait for the lock to be released (took %v)", elapsed)
	}
}

// TestLocking_DDLLockTimeoutConfiguredOnRetry guards against overwriting
// cfg.LockTimeout for the acquisition budget: cfg.LockTimeout also feeds
// SET lock_timeout for DDL, so a table-level lock conflict on the DDL itself
// must still fail fast with the configured timeout (a wrong fix that zeroes
// cfg.LockTimeout would disable the DDL timeout and hang here).
func TestLocking_DDLLockTimeoutConfiguredOnRetry(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_ddl_lock_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	initialSQL := "CREATE TABLE locked_tbl (id BIGINT PRIMARY KEY);"
	if err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Holder takes ACCESS EXCLUSIVE on the target table inside a transaction,
	// so the migration's DDL (ALTER TABLE) blocks on lock_timeout.
	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() {
		_, _ = holderConn.ExecContext(context.Background(), "ROLLBACK;")
		_ = holderConn.Close()
	}()
	if _, err := holderConn.ExecContext(context.Background(),
		fmt.Sprintf("BEGIN; LOCK TABLE %q.locked_tbl IN ACCESS EXCLUSIVE MODE;", schema)); err != nil {
		t.Fatalf("holder failed locking table: %v", err)
	}

	start := time.Now()
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:          grizzle.DialectPostgres,
		TargetSchema:     schema,
		SchemaSQL:        "CREATE TABLE locked_tbl (id BIGINT PRIMARY KEY, name TEXT);",
		AllowDrop:        true,
		LockTimeout:      300 * time.Millisecond,
		MaxRetries:       2,
		StatementTimeout: 5 * time.Second,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected DDL lock timeout failure while table lock is held, got nil")
	}
	if elapsed >= 3000*time.Millisecond {
		t.Errorf("DDL lock_timeout not honored across retries: took %v", elapsed)
	}
}

// TestLocking_LockReleasedAfterContextCancel verifies the advisory lock is
// released when the migration context is cancelled mid-migration, and that
// the dedicated connection is not handed back to the pool while it could
// still hold the lock (release failure discards it via driver.ErrBadConn).
func TestLocking_LockReleasedAfterContextCancel(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_cancel_rel_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	initialSQL := "CREATE TABLE cancels (id BIGINT PRIMARY KEY);"
	if err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	syncDone := make(chan error, 1)
	go func() {
		err := grizzle.Sync(ctx, db, grizzle.Options{
			Dialect:      grizzle.DialectPostgres,
			TargetSchema: schema,
			SchemaSQL:    initialSQL + "CREATE TABLE cancels2 (id BIGINT PRIMARY KEY);",
			BeforeStep: func(hc grizzle.HookContext) error {
				cancel() // cancel mid-migration, after the lock is held
				return nil
			},
		})
		syncDone <- err
	}()

	select {
	case <-syncDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("Sync did not return after mid-migration cancellation")
	}

	// The advisory lock must be free: acquiring it on a fresh connection
	// with a short timeout proves release.
	nsKey := postgres.Hash32("grizzle")
	schemaKey := postgres.Hash32(schema)
	testConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring test conn: %v", err)
	}
	defer func() { _ = testConn.Close() }()

	lockCtx, lockCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer lockCancel()
	if err := postgres.AcquireSessionAdvisoryLock2(lockCtx, testConn, nsKey, schemaKey); err != nil {
		t.Fatalf("advisory lock not released after context cancellation: %v", err)
	}
	var released bool
	_ = testConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1, $2);", nsKey, schemaKey).Scan(&released)
}

// TestHookPanic_LockReleasedAfterBeforeStepPanic verifies a panicking hook is
// recovered into an error (with stack) and the advisory lock is released.
func TestHookPanic_LockReleasedAfterBeforeStepPanic(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_hook_panic_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	initialSQL := "CREATE TABLE panic_tbl (id BIGINT PRIMARY KEY);"
	if err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL + "CREATE TABLE panic_tbl2 (id BIGINT PRIMARY KEY);",
		BeforeStep: func(hc grizzle.HookContext) error {
			panic("hook exploded")
		},
	})
	if err == nil {
		t.Fatalf("expected hook panic to surface as error, got nil")
	}
	if !strings.Contains(err.Error(), "hook panicked") || !strings.Contains(err.Error(), "hook exploded") {
		t.Errorf("expected panic message in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "goroutine") {
		t.Errorf("expected debug.Stack() in error, got: %v", err)
	}

	// Lock must be free after the panic.
	nsKey := postgres.Hash32("grizzle")
	schemaKey := postgres.Hash32(schema)
	testConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring test conn: %v", err)
	}
	defer func() { _ = testConn.Close() }()

	lockCtx, lockCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer lockCancel()
	if err := postgres.AcquireSessionAdvisoryLock2(lockCtx, testConn, nsKey, schemaKey); err != nil {
		t.Fatalf("advisory lock not released after hook panic: %v", err)
	}
	var released bool
	_ = testConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1, $2);", nsKey, schemaKey).Scan(&released)
}

// TestApply_FallbackUsesNamespaceLocks verifies the direct-apply fallback
// (plan without SchemaSQL) acquires namespace-derived locks, not an
// artifact-supplied LockID (which is never persisted).
func TestApply_FallbackUsesNamespaceLocks(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_fallback_lockid_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	initialSQL := "CREATE TABLE lockid_items (id BIGINT PRIMARY KEY);"
	if err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	desiredSQL := "CREATE TABLE lockid_items (id BIGINT PRIMARY KEY, name TEXT);"
	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
		LockTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Strip SchemaSQL to force the direct-apply fallback.
	p.SchemaSQL = ""
	approvedHash := p.Hash()

	// Hold the namespace+schema 2-int lock that Apply must use.
	nsKey := postgres.Hash32("grizzle")
	schemaKey := postgres.Hash32(schema)
	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() {
		var released bool
		_ = holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1, $2);", nsKey, schemaKey).Scan(&released)
		_ = holderConn.Close()
	}()

	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1, $2);", nsKey, schemaKey).Scan(&dummy); err != nil {
		t.Fatalf("holder failed acquiring namespace lock: %v", err)
	}

	applyDone := make(chan error, 1)
	go func() {
		applyDone <- grizzle.Apply(context.Background(), db, p, grizzle.ApplyOpts{
			ExpectedHash:  approvedHash,
			LockNamespace: "grizzle",
		})
	}()

	select {
	case err := <-applyDone:
		t.Fatalf("Apply bypassed namespace lock (returned while holder owns the lock): %v", err)
	case <-time.After(1200 * time.Millisecond):
		// still blocked — expected
	}

	var released bool
	if err := holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1, $2);", nsKey, schemaKey).Scan(&released); err != nil {
		t.Fatalf("holder unlock failed: %v", err)
	}

	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("Apply failed after lock release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Apply did not complete after lock release")
	}
}

// TestApply_FallbackRejectsOutOfScopeSteps verifies the direct-apply fallback
// enforces the plan's recorded IncludeTables/ExcludeTables scope against its
// steps, instead of blindly executing whatever the plan artifact contains.
func TestApply_FallbackRejectsOutOfScopeSteps(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_fallback_scope_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	initialSQL := "CREATE TABLE in_scope (id BIGINT PRIMARY KEY); CREATE TABLE out_of_scope (id BIGINT PRIMARY KEY);"
	if err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	desiredSQL := "CREATE TABLE in_scope (id BIGINT PRIMARY KEY, name TEXT);"
	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchema:  schema,
		SchemaSQL:     desiredSQL,
		IncludeTables: []string{"in_scope"},
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Tamper: inject a hazard-free step for a table outside the plan's
	// recorded scope. ADD COLUMN carries no hazard, so only scope enforcement
	// can reject it.
	p.Steps = append(p.Steps, grizzle.Step{
		Type:  grizzle.ChangeAddColumn,
		Table: "out_of_scope",
		SQL:   fmt.Sprintf("ALTER TABLE %q.out_of_scope ADD COLUMN sneaky TEXT;", schema),
	})
	p.SchemaSQL = ""

	err = grizzle.Apply(context.Background(), db, p, grizzle.ApplyOpts{ExpectedHash: p.Hash()})
	if err == nil {
		t.Fatalf("expected Apply to reject out-of-scope step, got nil")
	}
	if !strings.Contains(err.Error(), "outside the plan's recorded scope") {
		t.Errorf("expected scope violation error, got: %v", err)
	}

	// The out-of-scope table must be untouched.
	var cols int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'out_of_scope' AND column_name = 'sneaky';`, schema).Scan(&cols); err != nil {
		t.Fatalf("querying out_of_scope columns: %v", err)
	}
	if cols != 0 {
		t.Errorf("out-of-scope table was modified: sneaky column exists")
	}
}

// TestPlanDiff_ConcurrentCallsDoNotCollide verifies concurrent PlanDiff calls
// on one database each use their own shadow schema (no shared-name DDL
// serialization or CASCADE collision) and leave no shadow schemas behind.
func TestPlanDiff_ConcurrentCallsDoNotCollide(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_plandiff_conc_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	initialSQL := "CREATE TABLE conc_items (id BIGINT PRIMARY KEY);"
	if err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	desiredSQL := "CREATE TABLE conc_items (id BIGINT PRIMARY KEY, name TEXT);"
	shadowBase := fmt.Sprintf("_grizzle_shadow_test_%d", time.Now().UnixNano())

	const numCalls = 4
	var wg sync.WaitGroup
	errs := make(chan error, numCalls)
	plans := make(chan *grizzle.Plan, numCalls)
	barrier := make(chan struct{})

	for range numCalls {
		wg.Go(func() {
			<-barrier
			p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
				Dialect:      grizzle.DialectPostgres,
				TargetSchema: schema,
				SchemaSQL:    desiredSQL + "; SELECT pg_sleep(0.3);",
				ShadowSchema: shadowBase,
			})
			if err != nil {
				errs <- err
				return
			}
			plans <- p
		})
	}

	start := time.Now()
	close(barrier)
	wg.Wait()
	close(errs)
	elapsed := time.Since(start)

	for err := range errs {
		t.Errorf("concurrent PlanDiff error: %v", err)
	}

	// With per-call unique shadow schemas the compiles run in parallel;
	// sharing one name would serialize them on the shadow schema DDL locks
	// (4 x 300ms >= 1.2s serialized vs well under 1s parallel).
	if elapsed >= 2500*time.Millisecond {
		t.Errorf("concurrent PlanDiff calls appear serialized: took %v", elapsed)
	}

	first := <-plans
	for range numCalls - 1 {
		p := <-plans
		if p.Hash() != first.Hash() {
			t.Errorf("concurrent PlanDiff produced different hashes: %s vs %s", p.Hash(), first.Hash())
		}
	}

	// No shadow schema with the test prefix may survive.
	var leftovers int
	if err := db.QueryRow(fmt.Sprintf(
		"SELECT count(*) FROM pg_namespace WHERE nspname LIKE '%s%%';", shadowBase)).Scan(&leftovers); err != nil {
		t.Fatalf("querying pg_namespace: %v", err)
	}
	if leftovers != 0 {
		t.Errorf("expected 0 leftover shadow schemas, found %d", leftovers)
	}
}

// TestPlanDiff_CrashMidCompileLeavesNoShadow verifies that a compile that dies
// mid-flight (context deadline simulating a crashed/cancelled caller) leaves
// no stale shadow schema behind: the compile transaction aborts server-side.
func TestPlanDiff_CrashMidCompileLeavesNoShadow(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_plandiff_crash_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	initialSQL := "CREATE TABLE crash_items (id BIGINT PRIMARY KEY);"
	if err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	shadowBase := fmt.Sprintf("_grizzle_shadow_crash_%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// pg_sleep keeps the compile busy past the deadline.
	_, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    "CREATE TABLE crash_items (id BIGINT PRIMARY KEY); SELECT pg_sleep(5);",
		ShadowSchema: shadowBase,
	})
	if err == nil {
		t.Fatalf("expected PlanDiff to fail on cancelled context, got nil")
	}

	var leftovers int
	if err := db.QueryRow(fmt.Sprintf(
		"SELECT count(*) FROM pg_namespace WHERE nspname LIKE '%s%%';", shadowBase)).Scan(&leftovers); err != nil {
		t.Fatalf("querying pg_namespace: %v", err)
	}
	if leftovers != 0 {
		t.Errorf("expected 0 stale shadow schemas after mid-compile crash, found %d", leftovers)
	}
}
