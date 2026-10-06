package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
)

func getTestDSN() string {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		dsn = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}
	return dsn
}

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
	connStr := getTestDSN()
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
	connStr := getTestDSN()
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
	connStr := getTestDSN()
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

func TestLocking_DedicatedSessionAdvisoryLock_ContentionRetry(t *testing.T) {
	connStr := getTestDSN()
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

	lockID := grizzle.GenerateLockID("grizzle", schema)

	// 1. Holder connection grabs the dedicated session advisory lock directly
	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() { _ = holderConn.Close() }()

	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1);", lockID).Scan(&dummy); err != nil {
		t.Fatalf("holder failed acquiring pg_advisory_lock: %v", err)
	}

	// 2. Release holder lock after 150ms
	go func() {
		time.Sleep(150 * time.Millisecond)
		var released bool
		_ = holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1);", lockID).Scan(&released)
	}()

	// 3. Migration attempts Sync with a 50ms lock timeout and MaxRetries=5
	desiredSQL := `CREATE TABLE products (id BIGINT PRIMARY KEY, name TEXT);`
	start := time.Now()
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:          grizzle.DialectPostgres,
		TargetSchema:     schema,
		SchemaSQL:        desiredSQL,
		LockTimeout:      50 * time.Millisecond,
		StatementTimeout: 5 * time.Second,
		MaxRetries:       5,
	})
	if err != nil {
		t.Fatalf("migration failed despite retries on advisory lock contention: %v", err)
	}

	if time.Since(start) < 150*time.Millisecond {
		t.Errorf("migration should have waited for advisory lock release (expected >= 150ms, took %v)", time.Since(start))
	}
}

func TestLocking_DedicatedSessionAdvisoryLock_ExhaustRetriesFails(t *testing.T) {
	connStr := getTestDSN()
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

	lockID := grizzle.GenerateLockID("grizzle", schema)

	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() {
		var released bool
		_ = holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1);", lockID).Scan(&released)
		_ = holderConn.Close()
	}()

	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1);", lockID).Scan(&dummy); err != nil {
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

	if !exec.IsLockTimeout(err) && !strings.Contains(err.Error(), "lock") {
		t.Errorf("expected lock timeout error, got: %v", err)
	}
}

func TestLocking_DirectApply_DedicatedSessionLockAndNonTx(t *testing.T) {
	connStr := getTestDSN()
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
	connStr := getTestDSN()
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

	for i := 0; i < numPods; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier

			planCopy := *p
			if err := grizzle.Apply(context.Background(), db, &planCopy, grizzle.ApplyOpts{
				ExpectedHash: approvedHash,
			}); err != nil {
				errs <- err
			}
		}()
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
	connStr := getTestDSN()
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

	for i := 0; i < numPods; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier

			if err := grizzle.Sync(context.Background(), db, grizzle.Options{
				Dialect:      grizzle.DialectPostgres,
				TargetSchema: schema,
				SchemaSQL:    desiredSQL,
			}); err != nil {
				errs <- err
			}
		}()
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

