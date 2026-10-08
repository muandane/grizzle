//go:build integration

package exec_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/history"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

// captureHandler tracks log message records for asserting synchronization metrics.
type captureHandler struct {
	applied atomic.Int32
	noops   atomic.Int32
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *captureHandler) WithGroup(string) slog.Handler            { return h }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	switch r.Message {
	case "grizzle: synchronization finished successfully":
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "steps_applied" && a.Value.Int64() > 0 {
				h.applied.Add(1)
			}
			return true
		})
	case "grizzle: schema is already in sync":
		h.noops.Add(1)
	}
	return nil
}

func TestConcurrency_MultiPodRealPG(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_concurrency_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	migrationSQL := `
		CREATE TABLE customers (
			id BIGINT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE
		);

		CREATE TABLE orders (
			id BIGINT PRIMARY KEY,
			customer_id BIGINT REFERENCES customers(id),
			amount NUMERIC NOT NULL,
			created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		);

		CREATE INDEX idx_orders_customer ON orders(customer_id);
	`

	const numPods = 10
	var handler captureHandler
	logger := slog.New(&handler)

	var wg sync.WaitGroup
	errs := make(chan error, numPods)
	barrier := make(chan struct{})

	for i := 0; i < numPods; i++ {
		wg.Add(1)
		go func(podID int) {
			defer wg.Done()
			<-barrier // Synchronized barrier to hit PostgreSQL simultaneously

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			cfg := exec.PostgresExecConfig{
				TargetSchema: schema,
				ShadowSchema: fmt.Sprintf("%s_shadow_%d", schema, podID),
				SchemaSQL:    migrationSQL,
				LockID:       postgres.GenerateLockID(schema),
				Filters:      scope.Filters{},
				Policy: plan.DropPolicy{
					AllowTable:  false,
					AllowColumn: false,
					AllowIndex:  false,
					AllowFK:     false,
				},
				LockTimeout:      5 * time.Second,
				StatementTimeout: 30 * time.Second,
				MaxRetries:       5,
				Logger:           logger,
			}

			if err := exec.SyncPostgres(ctx, db, cfg); err != nil {
				errs <- fmt.Errorf("pod %d failed: %w", podID, err)
			}
		}(i)
	}

	close(barrier)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent migration error: %v", err)
	}

	appliedCount := handler.applied.Load()
	noopCount := handler.noops.Load()

	t.Logf("Concurrency results: exactly %d applied DDL, %d no-op", appliedCount, noopCount)

	if appliedCount != 1 {
		t.Errorf("expected exactly 1 pod to execute DDL, got %d", appliedCount)
	}
	if noopCount != numPods-1 {
		t.Errorf("expected %d pods to no-op, got %d", numPods-1, noopCount)
	}

	// Verify final schema on PostgreSQL
	var custTableCount, orderTableCount int
	err = db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '%s' AND table_name = 'customers';", schema)).Scan(&custTableCount)
	if err != nil || custTableCount != 1 {
		t.Errorf("expected customers table to exist, count=%d, err=%v", custTableCount, err)
	}

	err = db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '%s' AND table_name = 'orders';", schema)).Scan(&orderTableCount)
	if err != nil || orderTableCount != 1 {
		t.Errorf("expected orders table to exist, count=%d, err=%v", orderTableCount, err)
	}

	var validIndexCount int
	idxQuery := fmt.Sprintf(`
		SELECT COUNT(*)
		FROM pg_index ix
		JOIN pg_class t ON t.oid = ix.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = '%s' AND ix.indisvalid;
	`, schema)
	err = db.QueryRow(idxQuery).Scan(&validIndexCount)
	if err != nil || validIndexCount < 3 { // PK customers, unique email, PK orders, idx_orders_customer
		t.Errorf("expected valid indexes in schema, got %d, err=%v", validIndexCount, err)
	}
}

func TestConcurrency_LockTimeoutRetryAndConcurrent(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_lock_retry_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initialSQL := `
		CREATE TABLE inventory (
			id BIGINT PRIMARY KEY,
			stock INT NOT NULL
		);
	`
	initCfg := exec.PostgresExecConfig{
		TargetSchema: schema,
		ShadowSchema: fmt.Sprintf("%s_shadow_init", schema),
		SchemaSQL:    initialSQL,
		LockID:       postgres.GenerateLockID(schema),
	}
	if err := exec.SyncPostgres(context.Background(), db, initCfg); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Lock table in conflicting transaction
	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed opening holder conn: %v", err)
	}
	defer holderConn.Close()

	holderTx, err := holderConn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("failed starting holder tx: %v", err)
	}
	defer func() { _ = holderTx.Rollback() }()

	_, err = holderTx.Exec(fmt.Sprintf(`LOCK TABLE %s.inventory IN ACCESS EXCLUSIVE MODE;`, schema))
	if err != nil {
		t.Fatalf("failed acquiring exclusive lock: %v", err)
	}

	// Release lock after 150ms
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = holderTx.Rollback()
	}()

	desiredSQL := `
		CREATE TABLE inventory (
			id BIGINT PRIMARY KEY,
			stock INT NOT NULL,
			sku TEXT
		);
		CREATE INDEX idx_inventory_sku ON inventory(sku);
	`

	cfg := exec.PostgresExecConfig{
		TargetSchema: schema,
		ShadowSchema: fmt.Sprintf("%s_shadow_retry", schema),
		SchemaSQL:    desiredSQL,
		LockID:       postgres.GenerateLockID(schema),
		// LockTimeout bounds the TOTAL advisory-lock wait across retries;
		// holder releases at 150ms, so the budget must exceed that.
		LockTimeout:      1 * time.Second,
		StatementTimeout: 10 * time.Second,
		MaxRetries:       5,
	}

	err = exec.SyncPostgres(context.Background(), db, cfg)
	if err != nil {
		t.Fatalf("SyncPostgres failed despite lock timeout retry: %v", err)
	}
}

func TestConcurrency_LockCoverageAcrossTxAndNonTxGroups(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_lock_cov_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	// Schema needing both a table (tx group) and a concurrent index (non-tx group)
	desiredSQL := `
		CREATE TABLE articles (
			id BIGINT PRIMARY KEY,
			title TEXT NOT NULL,
			slug TEXT NOT NULL
		);
		CREATE INDEX idx_articles_slug ON articles(slug);
	`

	const numPods = 10
	var wg sync.WaitGroup
	errs := make(chan error, numPods)
	barrier := make(chan struct{})

	for i := 0; i < numPods; i++ {
		wg.Add(1)
		go func(podID int) {
			defer wg.Done()
			<-barrier // Launch all 10 pods simultaneously

			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()

			cfg := exec.PostgresExecConfig{
				TargetSchema: schema,
				ShadowSchema: fmt.Sprintf("%s_shad_%d", schema, podID),
				SchemaSQL:    desiredSQL,
				LockID:       postgres.GenerateLockID(schema),
				Filters:      scope.Filters{},
				Policy: plan.DropPolicy{
					AllowTable:  false,
					AllowColumn: false,
					AllowIndex:  false,
					AllowFK:     false,
				},
				LockTimeout:      5 * time.Second,
				StatementTimeout: 30 * time.Second,
				MaxRetries:       5,
			}

			if err := exec.SyncPostgres(ctx, db, cfg); err != nil {
				errs <- fmt.Errorf("pod %d failed: %w", podID, err)
			}
		}(i)
	}

	close(barrier)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("pod migration error: %v", err)
	}

	// Assert exactly ONE execution recorded in grizzle_history across all 10 pods
	records, err := history.List(context.Background(), db, "postgres", schema)
	if err != nil {
		t.Fatalf("failed reading history: %v", err)
	}

	if len(records) != 1 {
		t.Fatalf("expected exactly 1 history row executed across 10 pods, got %d records: %+v", len(records), records)
	}

	if records[0].Status != "applied" {
		t.Errorf("expected history record status 'applied', got %q", records[0].Status)
	}

	// Verify the table and the concurrent index exist and are valid
	var indexValid bool
	err = db.QueryRow(fmt.Sprintf(`
		SELECT ix.indisvalid
		FROM pg_index ix
		JOIN pg_class t ON t.oid = ix.indrelid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = '%s' AND i.relname = 'idx_articles_slug';
	`, schema)).Scan(&indexValid)
	if err != nil || !indexValid {
		t.Fatalf("expected idx_articles_slug to exist and be valid, err: %v, valid: %t", err, indexValid)
	}
}

type attemptTrackerHandler struct {
	attempts *atomic.Int32
}

func (h *attemptTrackerHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *attemptTrackerHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *attemptTrackerHandler) WithGroup(string) slog.Handler            { return h }
func (h *attemptTrackerHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "grizzle: starting schema synchronization" {
		h.attempts.Add(1)
	}
	return nil
}

func TestRetry_AbortAfterPartialProgress(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_retry_part_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initSQL := `CREATE TABLE accounts (id BIGINT PRIMARY KEY, status TEXT);`
	err = exec.SyncPostgres(context.Background(), db, exec.PostgresExecConfig{
		TargetSchema: schema,
		ShadowSchema: fmt.Sprintf("%s_init_shad", schema),
		SchemaSQL:    initSQL,
		LockID:       postgres.GenerateLockID(schema),
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Lock accounts table in a conflicting transaction so step 2 (CREATE INDEX CONCURRENTLY on accounts)
	// will encounter lock timeout
	lockConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed opening lock conn: %v", err)
	}
	defer lockConn.Close()

	_, _ = lockConn.ExecContext(context.Background(), fmt.Sprintf("SET search_path TO %q, public;", schema))
	_, err = lockConn.ExecContext(context.Background(), "BEGIN; LOCK TABLE accounts IN SHARE UPDATE EXCLUSIVE MODE;")
	if err != nil {
		t.Fatalf("failed acquiring conflicting lock: %v", err)
	}
	defer func() {
		_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK;")
	}()

	// Desired schema: Group 1 creates ledger (tx group), Group 2 creates index on accounts (non-tx group)
	desiredSQL := `
		CREATE TABLE accounts (id BIGINT PRIMARY KEY, status TEXT);
		CREATE TABLE ledger (id BIGINT PRIMARY KEY, amount NUMERIC);
		CREATE INDEX idx_accounts_status ON accounts(status);
	`

	var attempts atomic.Int32
	logger := slog.New(&attemptTrackerHandler{attempts: &attempts})

	cfg := exec.PostgresExecConfig{
		TargetSchema:     schema,
		ShadowSchema:     fmt.Sprintf("%s_shad", schema),
		SchemaSQL:        desiredSQL,
		LockID:           postgres.GenerateLockID(schema),
		LockTimeout:      50 * time.Millisecond,
		StatementTimeout: 5 * time.Second,
		MaxRetries:       5,
		Logger:           logger,
	}

	err = exec.SyncPostgres(context.Background(), db, cfg)
	if err == nil {
		t.Fatalf("expected SyncPostgres to fail due to lock conflict on step 2, got nil")
	}

	// Crucial assertion: Must NOT retry after partial progress!
	// Attempts must be exactly 1!
	attemptCount := attempts.Load()
	if attemptCount > 1 {
		t.Errorf("expected 0 retries after partial progress (attempts=1), but got attempts=%d", attemptCount)
	}

	// Verify partial progress: table ledger was committed in group 1
	var ledgerExists bool
	err = db.QueryRow(fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = '%s' AND table_name = 'ledger');", schema)).Scan(&ledgerExists)
	if err != nil || !ledgerExists {
		t.Fatalf("expected ledger table to exist from committed group 1, err: %v", err)
	}

	// Verify history record has status 'partial'
	latest, err := history.GetLatest(context.Background(), db, "postgres", schema)
	if err != nil {
		t.Fatalf("failed reading history: %v", err)
	}
	if latest == nil {
		t.Fatalf("expected history record, got nil")
	}
	if latest.Status != "partial" {
		t.Errorf("expected history status 'partial', got %q", latest.Status)
	}
	if latest.FailedStep != 2 {
		t.Errorf("expected failed_step 2, got %d", latest.FailedStep)
	}
}

func TestFK_ValidateConstraintRunsInSeparateTxAfterCommit(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_fk_sep_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	// 1. Create parents and children tables
	initSQL := fmt.Sprintf(`
		CREATE TABLE %s.parents (id INT PRIMARY KEY);
		CREATE TABLE %s.children (id INT PRIMARY KEY, parent_id INT);
	`, schema, schema)
	_, err = db.Exec(initSQL)
	if err != nil {
		t.Fatalf("failed creating initial tables: %v", err)
	}

	// 2. Insert invalid row in children referencing non-existent parent 999
	_, err = db.Exec(fmt.Sprintf("INSERT INTO %s.children (id, parent_id) VALUES (1, 999);", schema))
	if err != nil {
		t.Fatalf("failed inserting invalid child row: %v", err)
	}

	// 3. Desired schema defines FK from children.parent_id -> parents.id
	desiredSQL := `
		CREATE TABLE parents (id INT PRIMARY KEY);
		CREATE TABLE children (
			id INT PRIMARY KEY,
			parent_id INT,
			CONSTRAINT fk_child_parent FOREIGN KEY (parent_id) REFERENCES parents(id)
		);
	`

	cfg := exec.PostgresExecConfig{
		TargetSchema:     schema,
		ShadowSchema:     fmt.Sprintf("_shadow_%s", schema),
		SchemaSQL:        desiredSQL,
		LockID:           time.Now().UnixNano(),
		Policy:           plan.DropPolicy{},
		LockTimeout:      5 * time.Second,
		StatementTimeout: 10 * time.Second,
	}

	// 4. SyncPostgres should fail on VALIDATE CONSTRAINT because parent 999 doesn't exist
	err = exec.SyncPostgres(context.Background(), db, cfg)
	if err == nil {
		t.Fatalf("expected SyncPostgres to fail on VALIDATE CONSTRAINT with invalid rows, got nil")
	}

	// 5. CRUCIAL ASSERTION:
	// Because ADD CONSTRAINT ... NOT VALID ran and committed in Tx 1 before VALIDATE in Tx 2,
	// the constraint MUST exist in PostgreSQL pg_constraint with convalidated = false!
	// (If they were in the same transaction, ADD CONSTRAINT would have rolled back and not exist at all).
	var convalidated bool
	query := fmt.Sprintf(`
		SELECT convalidated
		FROM pg_constraint
		WHERE conname = 'fk_child_parent'
		  AND conrelid = '%s.children'::regclass;
	`, schema)
	err = db.QueryRow(query).Scan(&convalidated)
	if err != nil {
		t.Fatalf("expected constraint fk_child_parent to exist in catalog (proving ADD NOT VALID committed in separate tx), err: %v", err)
	}
	if convalidated {
		t.Errorf("expected convalidated to be false before VALIDATE CONSTRAINT succeeds")
	}

	// 6. Repair the invalid data by inserting parent 999
	_, err = db.Exec(fmt.Sprintf("INSERT INTO %s.parents (id) VALUES (999);", schema))
	if err != nil {
		t.Fatalf("failed inserting parent 999: %v", err)
	}

	// 7. Run sync again: VALIDATE CONSTRAINT should now succeed!
	err = exec.SyncPostgres(context.Background(), db, cfg)
	if err != nil {
		t.Fatalf("expected SyncPostgres to succeed after data repair, got: %v", err)
	}

	// 8. Assert convalidated is now true
	err = db.QueryRow(query).Scan(&convalidated)
	if err != nil {
		t.Fatalf("failed querying constraint after repair: %v", err)
	}
	if !convalidated {
		t.Errorf("expected convalidated to be true after successful validation")
	}
}
