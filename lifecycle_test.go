package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/history"
)

func TestLifecycle_StatusAndListHistory(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	schemaSQL := "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL);"

	// 1. Initial Status on fresh DB with no history
	stat, err := grizzle.Status(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
	})
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if stat.InSync {
		t.Errorf("expected InSync=false before sync")
	}
	if stat.PendingSteps != 1 {
		t.Errorf("expected 1 pending step, got %d", stat.PendingSteps)
	}
	if stat.AppliedCount != 0 {
		t.Errorf("expected AppliedCount=0, got %d", stat.AppliedCount)
	}
	if stat.Latest != nil {
		t.Errorf("expected nil Latest, got %+v", stat.Latest)
	}

	// 2. Sync to apply schema
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
	})
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Status after sync
	stat, err = grizzle.Status(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
	})
	if err != nil {
		t.Fatalf("Status after sync failed: %v", err)
	}
	if !stat.InSync {
		t.Errorf("expected InSync=true after sync")
	}
	if stat.PendingSteps != 0 {
		t.Errorf("expected 0 pending steps, got %d", stat.PendingSteps)
	}
	if stat.AppliedCount != 1 {
		t.Errorf("expected AppliedCount=1, got %d", stat.AppliedCount)
	}
	if stat.Latest == nil {
		t.Fatalf("expected non-nil Latest after sync")
	}
	if stat.Latest.Status != "applied" {
		t.Errorf("expected latest status 'applied', got %s", stat.Latest.Status)
	}

	// 3. ListHistory
	entries, err := grizzle.ListHistory(ctx, db, grizzle.HistoryOptions{
		Limit:      10,
		Descending: true,
	})
	if err != nil {
		t.Fatalf("ListHistory failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(entries))
	}
	if entries[0].ID != stat.Latest.ID {
		t.Errorf("expected entry ID %d, got %d", stat.Latest.ID, entries[0].ID)
	}
}

func TestLifecycle_Baseline(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Pre-populate table manually to simulate pre-existing database
	_, err = db.Exec("CREATE TABLE legacy (id INTEGER PRIMARY KEY, note TEXT);")
	if err != nil {
		t.Fatalf("failed creating legacy table: %v", err)
	}

	// 1. Baseline with empty desired schema returns ErrEmptySchema
	_, err = grizzle.Baseline(ctx, db, "", grizzle.BaselineOptions{
		Note:  "initial baseline",
		Force: false,
	})
	if !errors.Is(err, grizzle.ErrEmptySchema) {
		t.Fatalf("expected ErrEmptySchema on empty schemaSQL, got %v", err)
	}

	// Baseline with mismatching schema should fail without Force because legacy != users
	_, err = grizzle.Baseline(ctx, db, "CREATE TABLE users (id INTEGER PRIMARY KEY);", grizzle.BaselineOptions{
		Note:  "mismatching baseline",
		Force: false,
	})
	if !errors.Is(err, grizzle.ErrBaselineDrift) {
		t.Fatalf("expected ErrBaselineDrift when diff exists, got %v", err)
	}

	// 2. Baseline matching schema with Force=false should succeed
	matchingSQL := "CREATE TABLE legacy (id INTEGER PRIMARY KEY, note TEXT);"
	entry, err := grizzle.Baseline(ctx, db, matchingSQL, grizzle.BaselineOptions{
		Note:  "clean baseline",
		Force: false,
	})
	if err != nil {
		t.Fatalf("Baseline matching schema failed: %v", err)
	}
	if entry.Status != "baseline" {
		t.Errorf("expected status 'baseline', got %s", entry.Status)
	}

	// Status now shows in sync
	stat, err := grizzle.Status(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: matchingSQL,
	})
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if !stat.InSync {
		t.Errorf("expected InSync=true after baseline")
	}
	if stat.AppliedCount != 1 {
		t.Errorf("expected AppliedCount=1, got %d", stat.AppliedCount)
	}

	// 3. Baseline with Force=true should succeed even with unapplied schema
	unappliedSQL := matchingSQL + "\nCREATE TABLE extra (id INTEGER PRIMARY KEY);"
	forceEntry, err := grizzle.Baseline(ctx, db, unappliedSQL, grizzle.BaselineOptions{
		Note:  "forced baseline",
		Force: true,
	})
	if err != nil {
		t.Fatalf("forced baseline failed: %v", err)
	}
	if forceEntry.Status != "baseline" {
		t.Errorf("expected status 'baseline', got %s", forceEntry.Status)
	}
}

func TestLifecycle_Repair(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// 1. Repair on clean/empty history should return ErrNoFailedMigration
	_, err = grizzle.Repair(ctx, db, grizzle.RepairOptions{
		Note: "no failure to repair",
	})
	if !errors.Is(err, grizzle.ErrNoFailedMigration) {
		t.Fatalf("expected ErrNoFailedMigration, got %v", err)
	}

	// 2. Insert a failed migration record
	if err := history.EnsureTable(ctx, db, "sqlite", ""); err != nil {
		t.Fatalf("EnsureTable failed: %v", err)
	}
	err = history.Insert(ctx, db, "sqlite", "", history.Record{
		PlanHash:   "broken_hash",
		Status:     "failed",
		FailedStep: 1,
		Error:      "table already exists",
		AppliedAt:  time.Now().UTC(),
		DurationMs: 12,
		AppliedBy:  "operator",
		StepsJSON:  "[]",
	})
	if err != nil {
		t.Fatalf("Insert failed migration record: %v", err)
	}

	// Check status shows failed
	stat, err := grizzle.Status(ctx, db, grizzle.Options{
		Dialect: grizzle.DialectSQLite,
	})
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if stat.FailedCount != 1 {
		t.Errorf("expected FailedCount=1, got %d", stat.FailedCount)
	}

	// 3. Repair the failed migration
	repaired, err := grizzle.Repair(ctx, db, grizzle.RepairOptions{
		Note: "resolved table conflict manually",
	})
	if err != nil {
		t.Fatalf("Repair failed: %v", err)
	}
	if repaired.Status != "resolved" {
		t.Errorf("expected status 'resolved', got %s", repaired.Status)
	}

	// 4. Second repair should now return ErrNoFailedMigration
	_, err = grizzle.Repair(ctx, db, grizzle.RepairOptions{
		Note: "second repair",
	})
	if !errors.Is(err, grizzle.ErrNoFailedMigration) {
		t.Fatalf("expected ErrNoFailedMigration after resolution, got %v", err)
	}

	// Status now shows 0 failed
	stat, err = grizzle.Status(ctx, db, grizzle.Options{
		Dialect: grizzle.DialectSQLite,
	})
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if stat.FailedCount != 0 {
		t.Errorf("expected FailedCount=0 after repair, got %d", stat.FailedCount)
	}
	if stat.AppliedCount != 1 {
		t.Errorf("expected AppliedCount=1 (resolved counted as applied), got %d", stat.AppliedCount)
	}
}

func TestLifecycle_DumpRoundTrip(t *testing.T) {
	db1, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer func() { _ = db1.Close() }()

	ctx := context.Background()

	// Create tables and indexes on db1
	initSQL := `
		CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT NOT NULL);
		CREATE UNIQUE INDEX idx_users_email ON users(email);
		CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER, title TEXT, FOREIGN KEY(user_id) REFERENCES users(id));
	`
	if _, err := db1.ExecContext(ctx, initSQL); err != nil {
		t.Fatalf("failed creating initial tables: %v", err)
	}

	// Dump DDL from db1
	dumpedDDL, err := grizzle.Dump(ctx, db1, grizzle.DumpOptions{})
	if err != nil {
		t.Fatalf("Dump failed: %v", err)
	}
	if dumpedDDL == "" {
		t.Fatalf("expected non-empty dumped DDL")
	}

	// Create fresh db2 and apply the dumped DDL
	db2, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open db2: %v", err)
	}
	defer func() { _ = db2.Close() }()

	if _, err := db2.ExecContext(ctx, dumpedDDL); err != nil {
		t.Fatalf("failed executing dumped DDL on db2: %v\nDDL:\n%s", err, dumpedDDL)
	}

	// Verify db2 is 100% in sync with dumpedDDL (PlanDiff produces 0 steps)
	diffPlan, err := grizzle.PlanDiff(ctx, db2, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: dumpedDDL,
	})
	if err != nil {
		t.Fatalf("PlanDiff on db2 failed: %v", err)
	}
	if len(diffPlan.Steps) != 0 {
		t.Errorf("expected 0 steps diff on round-trip, got %d steps: %+v", len(diffPlan.Steps), diffPlan.Steps)
	}
}
