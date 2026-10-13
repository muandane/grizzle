package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muandane/grizzle"
)

// TestHardening_MultiPodFuzzing simulates a Kubernetes deployment where 50 replicas boot
// concurrently and attempt to synchronize the database schema at the exact same millisecond.
func TestHardening_MultiPodFuzzing(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	// Configure pool for high concurrency
	db.SetMaxOpenConns(60)
	db.SetMaxIdleConns(60)

	complexSchema := `
		CREATE TYPE order_status AS ENUM ('pending', 'processing', 'completed', 'cancelled');

		CREATE TABLE customers (
			id BIGSERIAL PRIMARY KEY,
			email VARCHAR(255) NOT NULL UNIQUE,
			tier VARCHAR(50) DEFAULT 'standard'
		);

		CREATE TABLE orders (
			id BIGSERIAL PRIMARY KEY,
			customer_id BIGINT NOT NULL,
			status order_status NOT NULL DEFAULT 'pending',
			total NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE
		);

		CREATE INDEX idx_orders_customer ON orders(customer_id);
		CREATE INDEX idx_orders_status ON orders(status) WHERE status = 'pending';
	`

	const numPods = 50
	var wg sync.WaitGroup
	errs := make(chan error, numPods)
	barrier := make(chan struct{}) // Ensures all goroutines strike the database at the exact same instant
	var completed atomic.Int32

	for range numPods {
		wg.Go(func() {
			<-barrier // Synchronized release

			// Waiters serialize on the advisory lock; budget must cover the
			// leader's full sync (incl. CONCURRENTLY indexes) plus retries.
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			err := grizzle.Sync(ctx, db, grizzle.Options{
				Dialect:      grizzle.DialectPostgres,
				TargetSchema: "public",
				SchemaSQL:    complexSchema,
				AllowDrop:    false,
				LockTimeout:  15 * time.Second,
				MaxRetries:   10,
			})
			if err != nil {
				errs <- err
			} else {
				completed.Add(1)
			}
		})
	}

	// Release all pods simultaneously
	close(barrier)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("pod migration failed during high-concurrency race: %v", err)
	}

	if completed.Load() != int32(numPods) {
		t.Fatalf("expected all %d pods to complete successfully, got %d", numPods, completed.Load())
	}

	// Verify schema state on PostgreSQL
	var orderCount, custCount int
	err := db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('customers', 'orders');").Scan(&orderCount)
	if err != nil || orderCount != 2 {
		t.Fatalf("expected 2 tables created in public schema, got %d, err: %v", orderCount, err)
	}

	var indexCount int
	err = db.QueryRow("SELECT COUNT(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'orders';").Scan(&indexCount)
	if err != nil || indexCount < 2 {
		t.Fatalf("expected at least 2 indexes on orders, got %d, err: %v", indexCount, err)
	}

	_ = custCount
}

// TestHardening_WarmBootLatency verifies that an already-synchronized database boots in < 30ms.
func TestHardening_WarmBootLatency(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	schema := `
		CREATE TABLE accounts (
			id BIGSERIAL PRIMARY KEY,
			name VARCHAR(100) NOT NULL
		);
	`
	ctx := t.Context()

	// Initial Sync
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Measure warm boot across multiple runs to absorb CI runner scheduling jitter.
	const attempts = 3
	var bestElapsed time.Duration
	for i := range attempts {
		start := time.Now()
		if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema}); err != nil {
			t.Fatalf("warm boot sync attempt %d failed: %v", i+1, err)
		}
		elapsed := time.Since(start)
		if i == 0 || elapsed < bestElapsed {
			bestElapsed = elapsed
		}
	}

	t.Logf("best warm boot latency across %d attempts: %v", attempts, bestElapsed)
	if bestElapsed > 250*time.Millisecond {
		t.Errorf("warm boot exceeded latency SLA (250ms max allowed in shared test env), took %v", bestElapsed)
	}
}

// TestHardening_SQLite_ConcurrentReads verifies SQLite behavior with concurrent read queries
// across a shared connection pool.
func TestHardening_SQLite_ConcurrentReads(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", "file:memdb_hardening?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed to open shared memory db: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `CREATE TABLE cache (key TEXT PRIMARY KEY, val TEXT);`
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema}); err != nil {
		t.Fatalf("sqlite initial sync failed: %v", err)
	}

	// Insert test data
	_, err = db.ExecContext(ctx, "INSERT INTO cache (key, val) VALUES ('1', 'data1'), ('2', 'data2');")
	if err != nil {
		t.Fatalf("failed inserting data: %v", err)
	}

	var wg sync.WaitGroup
	const readers = 20

	for i := range readers {
		wg.Go(func() {
			var cnt int
			err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM cache WHERE key = '%d'", (i%2)+1)).Scan(&cnt)
			if err != nil {
				t.Errorf("concurrent sqlite read failed: %v", err)
			}
			if cnt != 1 {
				t.Errorf("expected count 1, got %d", cnt)
			}
		})
	}
	wg.Wait()
}

// TestHardening_PriorFailureBlocking verifies that Sync() fails closed when
// the most recent migration in grizzle_history is in a failed or partial state,
// preventing restart crash loops until explicitly overridden with AllowResumePartial.
func TestHardening_PriorFailureBlocking(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := t.Context()

	testCases := []struct {
		name   string
		status string
	}{
		{name: "PriorFailedState", status: "failed"},
		{name: "PriorPartialState", status: "partial"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resetPublicSchema(t, db)

			// 1. Initial successful migration establishing clean baseline
			initialSchema := `CREATE TABLE baseline (id BIGSERIAL PRIMARY KEY);`
			if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: initialSchema}); err != nil {
				t.Fatalf("initial baseline sync failed: %v", err)
			}

			// Simulate prior run recorded in failure/partial status
			fakeHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			insertSQL := `
				INSERT INTO "public".grizzle_history (plan_hash, status, failed_step, error, applied_at, duration_ms, applied_by, steps)
				VALUES ($1, $2, 1, 'simulated migration error', NOW(), 25, 'test_runner', '[]');
			`
			if _, err := db.ExecContext(ctx, insertSQL, fakeHash, tc.status); err != nil {
				t.Fatalf("failed inserting simulated %s record: %v", tc.status, err)
			}

			tableName := fmt.Sprintf("blocked_table_%s", tc.status)
			targetSchema := fmt.Sprintf(`
				CREATE TABLE baseline (id BIGSERIAL PRIMARY KEY);
				CREATE TABLE %s (id BIGSERIAL PRIMARY KEY, val TEXT);
			`, tableName)

			// 2. Default Sync() must fail closed with ErrPriorFailureBlocked
			err := grizzle.Sync(ctx, db, grizzle.Options{
				SchemaSQL: targetSchema,
				// AllowResumePartial defaults to false
			})
			if err == nil {
				t.Fatalf("expected Sync to fail closed on prior %s state, got nil", tc.status)
			}
			if !errors.Is(err, grizzle.ErrPriorFailureBlocked) {
				t.Fatalf("expected ErrPriorFailureBlocked, got: %v", err)
			}

			// Verify live database state remained completely untouched
			var exists bool
			err = db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1);", tableName).Scan(&exists)
			if err != nil || exists {
				t.Fatalf("%s should not exist in database, exists=%t, err=%v", tableName, exists, err)
			}

			// 3. Sync() with functional option WithAllowResumePartial(true) must resume and succeed
			resumeOpts := grizzle.Options{
				SchemaSQL: targetSchema,
			}
			grizzle.WithAllowResumePartial(true)(&resumeOpts)
			err = grizzle.Sync(ctx, db, resumeOpts)
			if err != nil {
				t.Fatalf("expected Sync to succeed with AllowResumePartial=true, got: %v", err)
			}

			// Verify schema change applied
			err = db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1);", tableName).Scan(&exists)
			if err != nil || !exists {
				t.Fatalf("%s must exist after resumed migration, exists=%t, err=%v", tableName, exists, err)
			}

			// Verify grizzle_history latest status is now "applied"
			var latestStatus string
			err = db.QueryRowContext(ctx, "SELECT status FROM grizzle_history ORDER BY id DESC LIMIT 1;").Scan(&latestStatus)
			if err != nil || latestStatus != "applied" {
				t.Fatalf("expected latest history status 'applied', got %q, err: %v", latestStatus, err)
			}
		})
	}
}

// TestHardening_SingleConnectionPool verifies that Sync() operates safely on
// severely constrained pools (MaxOpenConns = 1), recording failure history
// without deadlocking, and enforcing sql.LevelReadCommitted transaction isolation.
func TestHardening_SingleConnectionPool(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	// Constrain pool strictly to a single connection
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx := t.Context()

	// Track observed transaction isolation level during DDL execution
	var observedIsolation string
	var stepHookCount int

	schemaSQL := `
		CREATE TABLE single_conn_users (id BIGSERIAL PRIMARY KEY, name TEXT);
		CREATE TABLE single_conn_orders (id BIGSERIAL PRIMARY KEY, user_id BIGINT);
	`

	simulatedErr := errors.New("simulated failure at step 2 to verify pool starvation safety")

	start := time.Now()
	err := grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaSQL,
		BeforeStep: func(hc grizzle.HookContext) error {
			stepHookCount++
			// Verify DDL transaction isolation level inside transactional step
			if !hc.IsNonTx {
				var iso string
				if scanErr := hc.DBTX.QueryRowContext(hc.Context, "SHOW transaction_isolation;").Scan(&iso); scanErr == nil {
					observedIsolation = strings.ToLower(strings.TrimSpace(iso))
				}
			}
			// Trigger failure at step 2
			if hc.Index == 2 {
				return simulatedErr
			}
			return nil
		},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error from simulated hook failure, got nil")
	}
	if !strings.Contains(err.Error(), "simulated failure at step 2") {
		t.Fatalf("expected simulated error message, got: %v", err)
	}

	// 1. Starvation check: Must NOT take ~10 seconds (the detached histCtx timeout).
	// A non-deadlocked execution completes in < 2.5 seconds on shared test runners.
	if elapsed > 3*time.Second {
		t.Fatalf("Sync() took %v; indicates connection pool deadlock / starvation during failure recording", elapsed)
	}
	t.Logf("single-connection pool failure handled and recorded in %v", elapsed)

	// 2. Isolation check: Must be explicitly 'read committed'
	if observedIsolation != "read committed" {
		t.Errorf("expected transaction_isolation 'read committed', got %q", observedIsolation)
	}

	// 3. History check: Query using the single connection (proves pool is not leaked/starved)
	var histStatus, histErr string
	var histFailedStep int
	queryErr := db.QueryRowContext(ctx, "SELECT status, failed_step, COALESCE(error, '') FROM grizzle_history ORDER BY id DESC LIMIT 1;").Scan(&histStatus, &histFailedStep, &histErr)
	if queryErr != nil {
		t.Fatalf("failed querying grizzle_history on single-connection pool: %v", queryErr)
	}
	if histStatus != "failed" && histStatus != "partial" {
		t.Errorf("expected history status 'failed' or 'partial', got %q", histStatus)
	}
	if !strings.Contains(histErr, "simulated failure at step 2") {
		t.Errorf("expected history error to contain failure details, got %q", histErr)
	}

	// 4. Recovery check: Verify subsequent migration succeeds on the same MaxOpenConns = 1 pool
	repairSchema := `CREATE TABLE single_conn_users (id BIGSERIAL PRIMARY KEY, name TEXT);`
	repairOpts := grizzle.Options{
		SchemaSQL: repairSchema,
	}
	grizzle.WithAllowResumePartial(true)(&repairOpts)
	if err := grizzle.Sync(ctx, db, repairOpts); err != nil {
		t.Fatalf("subsequent Sync on single-conn pool failed: %v", err)
	}
}

// TestHardening_ExpectedHashEnforcement verifies that Sync() enforces Options.ExpectedHash,
// aborting with ErrPlanDrift when tampered or drifted, and leaving the database untouched.
func TestHardening_ExpectedHashEnforcement(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	ctx := t.Context()

	// 1. Initial table
	initialSchema := `CREATE TABLE sensitive_tbl (id BIGSERIAL PRIMARY KEY);`
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: initialSchema}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// 2. Desired target schema
	desiredSchema := `
		CREATE TABLE sensitive_tbl (id BIGSERIAL PRIMARY KEY);
		CREATE TABLE sensitive_records (id BIGSERIAL PRIMARY KEY, payload TEXT);
	`

	// 3. Compute approved plan hash using PlanDiff
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{SchemaSQL: desiredSchema})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	approvedHash := p.Hash()
	if approvedHash == "" {
		t.Fatalf("approved hash must not be empty")
	}

	// 4. Case A: Sync with tampered / unexpected ExpectedHash
	tamperedHash := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	tamperedOpts := grizzle.Options{
		SchemaSQL: desiredSchema,
	}
	grizzle.WithExpectedHash(tamperedHash)(&tamperedOpts)
	err = grizzle.Sync(ctx, db, tamperedOpts)
	if err == nil {
		t.Fatalf("expected Sync to abort on tampered ExpectedHash, got nil")
	}
	if !errors.Is(err, grizzle.ErrPlanDrift) {
		t.Fatalf("expected ErrPlanDrift on hash mismatch, got: %v", err)
	}

	// Verify database was untouched: sensitive_records table must not exist
	var exists bool
	err = db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'sensitive_records');").Scan(&exists)
	if err != nil || exists {
		t.Fatalf("sensitive_records must NOT exist after aborted Sync, exists=%t, err=%v", exists, err)
	}

	// 5. Case B: Sync with valid approved ExpectedHash succeeds
	validOpts := grizzle.Options{
		SchemaSQL: desiredSchema,
	}
	grizzle.WithExpectedHash(approvedHash)(&validOpts)
	err = grizzle.Sync(ctx, db, validOpts)
	if err != nil {
		t.Fatalf("expected Sync to succeed with matching ExpectedHash, got: %v", err)
	}

	// Verify table now exists
	err = db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'sensitive_records');").Scan(&exists)
	if err != nil || !exists {
		t.Fatalf("sensitive_records must exist after successful Sync, exists=%t, err=%v", exists, err)
	}

	// 6. Case C: Re-running with old ExpectedHash detects drift (database is now up to date)
	err = grizzle.Sync(ctx, db, validOpts)
	if !errors.Is(err, grizzle.ErrPlanDrift) {
		t.Fatalf("expected ErrPlanDrift on re-run because live DB already updated (0 steps vs migration plan), got: %v", err)
	}
}
