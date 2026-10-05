package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/history"
)

func TestDrift_CheckDetectsDifferenceWithoutApplying(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer db.Close()

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
	defer db.Close()

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
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer db.Close()

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
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer db.Close()

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

	// Hold a conflicting lock on items table so CREATE INDEX CONCURRENTLY will block
	lockConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring lock conn: %v", err)
	}
	defer lockConn.Close()

	_, err = lockConn.ExecContext(context.Background(), fmt.Sprintf("SET search_path TO %q, public;", schema))
	if err != nil {
		t.Fatalf("failed setting search_path on lock conn: %v", err)
	}
	_, err = lockConn.ExecContext(context.Background(), "BEGIN; LOCK TABLE items IN SHARE UPDATE EXCLUSIVE MODE;")
	if err != nil {
		t.Fatalf("failed acquiring conflicting lock: %v", err)
	}

	// Execute Apply with a short cancel
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	applyErr := grizzle.Apply(ctx, db, plan, grizzle.ApplyOpts{})
	if applyErr == nil {
		t.Fatalf("expected Apply to fail when cancelled mid non-tx step, got nil")
	}

	// Release conflicting lock
	_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK;")

	// Query grizzle_history using a fresh context
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

