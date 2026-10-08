package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/history"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestDrift_CheckDetectsDifferenceWithoutApplying(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	initialSQL := "CREATE TABLE users (id INTEGER PRIMARY KEY);"

	// Sync initial schema
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: initialSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// 1. Desired schema has an extra table
	driftSQL := `
		CREATE TABLE users (id INTEGER PRIMARY KEY);
		CREATE TABLE orders (id INTEGER PRIMARY KEY);
	`
	opts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: driftSQL,
	}

	// Check should return ErrDrift / DriftError
	err = grizzle.Check(ctx, db, opts)
	if err == nil {
		t.Fatalf("expected Check to detect drift, got nil")
	}
	if !errors.Is(err, grizzle.ErrDrift) {
		t.Errorf("expected errors.Is(err, ErrDrift), got %v", err)
	}

	var driftErr *grizzle.DriftError
	if !errors.As(err, &driftErr) {
		t.Fatalf("expected errors.As(err, &DriftError), got %v", err)
	}
	if len(driftErr.Plan.Steps) != 1 {
		t.Errorf("expected 1 drift step, got %d", len(driftErr.Plan.Steps))
	}

	// Verify Check was read-only and did NOT apply the change
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='orders';").Scan(&count)
	if err != nil || count != 0 {
		t.Errorf("expected orders table to NOT exist after Check, count=%d, err=%v", count, err)
	}

	// 2. When schemas match, Check should return nil
	matchOpts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: initialSQL,
	}
	err = grizzle.Check(ctx, db, matchOpts)
	if err != nil {
		t.Errorf("expected Check to return nil when schemas match, got %v", err)
	}
}

func TestHistory_RecordedOnSync(t *testing.T) {
	// Test on SQLite
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	schemaSQL := "CREATE TABLE products (id INTEGER PRIMARY KEY, title TEXT);"

	opts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
	}

	err = grizzle.Sync(ctx, db, opts)
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Query grizzle_history
	latest, err := history.GetLatest(ctx, db, "sqlite", "")
	if err != nil {
		t.Fatalf("failed reading history: %v", err)
	}
	if latest == nil {
		t.Fatalf("expected history entry to be recorded on Sync")
	}

	p, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	// Plan when in sync has 0 steps, but original plan had 1 step
	if latest.StepsJSON == "" {
		t.Errorf("expected non-empty steps_json in history")
	}

	// Running sync again when schema is already in sync should NOT add another history record
	err = grizzle.Sync(ctx, db, opts)
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}

	records, err := history.List(ctx, db, "sqlite", "")
	if err != nil {
		t.Fatalf("failed listing history: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("expected exactly 1 history record, got %d", len(records))
	}
	_ = p
}

func TestHistory_PostgresRecording(t *testing.T) {
	connStr := testutil.PostgresDSN()

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres history test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_history_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	schemaSQL := `CREATE TABLE invoices (id BIGINT PRIMARY KEY, total NUMERIC);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    schemaSQL,
	})
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Verify history table written in same schema
	latest, err := history.GetLatest(context.Background(), db, "postgres", schema)
	if err != nil {
		t.Fatalf("failed fetching postgres history: %v", err)
	}
	if latest == nil {
		t.Fatalf("expected non-nil history record in postgres")
	}
	if latest.DurationMs < 0 {
		t.Errorf("expected duration_ms >= 0, got %d", latest.DurationMs)
	}
	if latest.AppliedBy == "" {
		t.Errorf("expected non-empty applied_by")
	}
}

func TestHistory_PartialOnKilledNonTxStep(t *testing.T) {
	connStr := testutil.PostgresDSN()

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres history test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_hist_part_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initSQL := `CREATE TABLE items (id BIGINT PRIMARY KEY, name TEXT);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Desired schema adds an index (which postgres planner emits as CREATE INDEX CONCURRENTLY - non-tx)
	desiredSQL := `
		CREATE TABLE items (id BIGINT PRIMARY KEY, name TEXT);
		CREATE INDEX idx_items_name ON items (name);
	`
	plan, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Assert the plan has a non-tx step
	var hasNonTx bool
	for _, s := range plan.Steps {
		if s.NonTx {
			hasNonTx = true
			break
		}
	}
	if !hasNonTx {
		t.Fatalf("expected plan to have a non-tx step, got: %+v", plan.Steps)
	}

	// Hold a conflicting lock so CREATE INDEX CONCURRENTLY blocks in the non-tx step.
	// A fixed-time context cancel races: if it fires during advisory-lock acquire,
	// Apply returns an error without writing history, and GetLatest still sees the
	// initial sync's "applied" row (flake seen on PG 15 CI). Wait until the CIC
	// query is visible, then terminate that backend so failure is always mid-non-tx.
	lockConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring lock conn: %v", err)
	}
	defer func() { _ = lockConn.Close() }()

	_, err = lockConn.ExecContext(context.Background(), fmt.Sprintf("SET search_path TO %q, public;", schema))
	if err != nil {
		t.Fatalf("failed setting search_path on lock conn: %v", err)
	}
	_, err = lockConn.ExecContext(context.Background(), "BEGIN; LOCK TABLE items IN SHARE UPDATE EXCLUSIVE MODE;")
	if err != nil {
		t.Fatalf("failed acquiring conflicting lock: %v", err)
	}
	defer func() {
		_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK;")
	}()

	applyDone := make(chan error, 1)
	go func() {
		applyDone <- grizzle.Apply(context.Background(), db, plan, grizzle.ApplyOpts{})
	}()

	terminateCtx, cancelTerminate := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelTerminate()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	terminated := false
	for !terminated {
		select {
		case <-terminateCtx.Done():
			t.Fatalf("timed out waiting for CREATE INDEX CONCURRENTLY to start so it can be killed")
		case applyErr := <-applyDone:
			t.Fatalf("Apply finished before non-tx backend could be terminated: %v", applyErr)
		case <-ticker.C:
			var pid int
			findPID := `
				SELECT pid FROM pg_stat_activity
				WHERE state = 'active'
				  AND datname = current_database()
				  AND query LIKE '%CREATE INDEX CONCURRENTLY%idx_items_name%'
				  AND pid <> pg_backend_pid()
				LIMIT 1`
			if err := db.QueryRowContext(terminateCtx, findPID).Scan(&pid); err != nil || pid <= 0 {
				continue
			}
			var ok bool
			if err := db.QueryRowContext(terminateCtx, "SELECT pg_terminate_backend($1);", pid).Scan(&ok); err != nil {
				t.Fatalf("pg_terminate_backend(%d): %v", pid, err)
			}
			if !ok {
				t.Fatalf("pg_terminate_backend(%d) returned false", pid)
			}
			terminated = true
		}
	}

	var applyErr error
	select {
	case applyErr = <-applyDone:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for Apply to return after backend terminate")
	}
	if applyErr == nil {
		t.Fatalf("expected Apply to fail when non-tx backend was terminated, got nil")
	}
	t.Logf("applyErr after terminate: %v", applyErr)

	latest, err := history.GetLatest(context.Background(), db, "postgres", schema)
	if err != nil {
		t.Fatalf("failed fetching latest history: %v", err)
	}
	if latest == nil {
		t.Fatalf("expected history record to be written on partial/failed non-tx run, but got nil")
	}

	if latest.Status != "partial" {
		t.Errorf("expected history status 'partial', got %q", latest.Status)
	}
	if latest.FailedStep <= 0 {
		t.Errorf("expected failed_step > 0, got %d", latest.FailedStep)
	}
	if latest.PlanHash != plan.Hash() {
		t.Errorf("expected plan hash %s, got %s", plan.Hash(), latest.PlanHash)
	}
	if latest.Error == "" {
		t.Errorf("expected non-empty error in history record")
	}
}
