package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/history"
	_ "modernc.org/sqlite"
)

func TestIsReady_SQLite_Lifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	// 1. Unmigrated database (grizzle_history does not exist)
	ready, err := grizzle.IsReady(ctx, db)
	if err != nil {
		t.Fatalf("unexpected error on unmigrated db: %v", err)
	}
	if ready {
		t.Errorf("expected ready=false on unmigrated db, got true")
	}

	// Verify no side effects: grizzle_history must NOT have been created by IsReady
	var count int
	err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='grizzle_history';").Scan(&count)
	if err != nil {
		t.Fatalf("checking table existence: %v", err)
	}
	if count != 0 {
		t.Errorf("grizzle.IsReady created table grizzle_history! Expected 0 side effects")
	}

	// 2. Applied migration
	schemaSQL := "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);"
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
	})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	ready, err = grizzle.IsReady(ctx, db)
	if err != nil {
		t.Fatalf("unexpected error after sync: %v", err)
	}
	if !ready {
		t.Errorf("expected ready=true after sync, got false")
	}

	// 3. Failed migration recorded in history
	_, err = db.Exec("INSERT INTO grizzle_history (plan_hash, status, duration_ms, applied_by, steps) VALUES ('hash_fail', 'failed', 10, 'test', '[]');")
	if err != nil {
		t.Fatalf("inserting failed history record: %v", err)
	}

	ready, err = grizzle.IsReady(ctx, db)
	if err != nil {
		t.Fatalf("unexpected error after failed history: %v", err)
	}
	if ready {
		t.Errorf("expected ready=false when latest record is 'failed', got true")
	}

	// 4. Closed database returns error
	_ = db.Close()
	ready, err = grizzle.IsReady(ctx, db)
	if err == nil {
		t.Errorf("expected error on closed db, got nil")
	}
	if ready {
		t.Errorf("expected ready=false on closed db, got true")
	}
}

func TestIsReady_Postgres_Lifecycle(t *testing.T) {
	db, _ := getPostgresDB(t)

	ctx := context.Background()
	schema := fmt.Sprintf("ready_test_%d", time.Now().UnixNano())

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %q;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE;", schema))
	}()

	// 1. Unmigrated schema: grizzle_history table does not exist
	ready, err := grizzle.IsReady(ctx, db, grizzle.WithTargetSchema(schema))
	if err != nil {
		t.Fatalf("unexpected error on unmigrated schema: %v", err)
	}
	if ready {
		t.Errorf("expected ready=false on unmigrated schema, got true")
	}

	// Verify no side effects: grizzle_history must NOT have been created by IsReady
	var tableCount int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.tables 
		WHERE table_schema = $1 AND table_name = 'grizzle_history';
	`, schema).Scan(&tableCount)
	if err != nil {
		t.Fatalf("checking table existence: %v", err)
	}
	if tableCount != 0 {
		t.Errorf("grizzle.IsReady created table grizzle_history! Expected 0 side effects")
	}

	// 2. Applied migration
	schemaSQL := "CREATE TABLE items (id INT PRIMARY KEY, title TEXT);"
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    schemaSQL,
	})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	ready, err = grizzle.IsReady(ctx, db, grizzle.WithTargetSchema(schema))
	if err != nil {
		t.Fatalf("unexpected error after sync: %v", err)
	}
	if !ready {
		t.Errorf("expected ready=true after successful sync, got false")
	}

	// 3. Failed migration record
	err = history.Insert(ctx, db, "postgres", schema, history.Record{
		PlanHash:   "hash_f",
		Status:     "failed",
		DurationMs: 15,
		AppliedBy:  "test",
		StepsJSON:  "[]",
	})
	if err != nil {
		t.Fatalf("inserting failed history: %v", err)
	}

	ready, err = grizzle.IsReady(ctx, db, grizzle.WithTargetSchema(schema))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Errorf("expected ready=false when latest status is failed, got true")
	}

	// 4. Baseline record recovers readiness
	err = history.Insert(ctx, db, "postgres", schema, history.Record{
		PlanHash:   "hash_b",
		Status:     "baseline",
		DurationMs: 10,
		AppliedBy:  "test",
		StepsJSON:  "[]",
	})
	if err != nil {
		t.Fatalf("inserting baseline history: %v", err)
	}

	ready, err = grizzle.IsReady(ctx, db, grizzle.WithTargetSchema(schema))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ready {
		t.Errorf("expected ready=true when latest status is baseline, got false")
	}
}

func TestIsReady_Postgres_ZeroLocksAcquired(t *testing.T) {
	db, _ := getPostgresDB(t)

	ctx := context.Background()
	schema := fmt.Sprintf("ready_lock_%d", time.Now().UnixNano())

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %q;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE;", schema))
	}()

	// Apply migration first so history exists
	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    "CREATE TABLE t1 (id INT PRIMARY KEY);",
	})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	// Call IsReady
	ready, err := grizzle.IsReady(ctx, db, grizzle.WithTargetSchema(schema))
	if err != nil || !ready {
		t.Fatalf("expected ready=true, err=nil, got ready=%v, err=%v", ready, err)
	}

	// Verify no advisory locks were held/leaked
	var advCount int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid();").Scan(&advCount)
	if err != nil {
		t.Fatalf("querying advisory locks: %v", err)
	}
	if advCount != 0 {
		t.Errorf("expected 0 advisory locks held by session, got %d", advCount)
	}
}

func TestIsReady_MultiSchema(t *testing.T) {
	db, _ := getPostgresDB(t)

	ctx := context.Background()
	schema1 := fmt.Sprintf("ready_ms1_%d", time.Now().UnixNano())
	schema2 := fmt.Sprintf("ready_ms2_%d", time.Now().UnixNano())

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %q; CREATE SCHEMA %q;", schema1, schema2)); err != nil {
		t.Fatalf("creating test schemas: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE; DROP SCHEMA %q CASCADE;", schema1, schema2))
	}()

	// Apply only to schema1
	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema1,
		SchemaSQL:    "CREATE TABLE t1 (id INT PRIMARY KEY);",
	})
	if err != nil {
		t.Fatalf("sync schema1 failed: %v", err)
	}

	// Multi-schema check: schema1 is ready, schema2 is not -> IsReady must return false
	ready, err := grizzle.IsReady(ctx, db, func(o *grizzle.Options) {
		o.TargetSchemas = []string{schema1, schema2}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Errorf("expected ready=false when one schema is unmigrated, got true")
	}

	// Now apply to schema2
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema2,
		SchemaSQL:    "CREATE TABLE t2 (id INT PRIMARY KEY);",
	})
	if err != nil {
		t.Fatalf("sync schema2 failed: %v", err)
	}

	// Both are now migrated -> IsReady must return true
	ready, err = grizzle.IsReady(ctx, db, func(o *grizzle.Options) {
		o.TargetSchemas = []string{schema1, schema2}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ready {
		t.Errorf("expected ready=true when all schemas are migrated, got false")
	}
}

func TestIsReady_Postgres_NoOpSyncWritesHash(t *testing.T) {
	db, _ := getPostgresDB(t)

	ctx := context.Background()
	schema := fmt.Sprintf("ready_noop_%d", time.Now().UnixNano())

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %q;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE;", schema))
	}()

	schemaSQL := "CREATE TABLE items (id INT PRIMARY KEY, title TEXT);"
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %q.items (id INT PRIMARY KEY, title TEXT);", schema)); err != nil {
		t.Fatalf("creating pre-existing table: %v", err)
	}

	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    schemaSQL,
	}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	ready, err := grizzle.IsReady(ctx, db,
		grizzle.WithDialect(grizzle.DialectPostgres),
		grizzle.WithTargetSchema(schema),
		grizzle.WithSchemaSQL(schemaSQL),
	)
	if err != nil {
		t.Fatalf("IsReady failed: %v", err)
	}
	if !ready {
		t.Fatal("expected IsReady true after no-op sync with matching schema")
	}
}
