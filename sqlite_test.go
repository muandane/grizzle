package grizzle_test

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

func getSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLite_EndToEnd(t *testing.T) {
	db := getSQLiteDB(t)
	ctx := t.Context()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Phase 1: Initial schema provisioning
	schemaV1 := `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT NOT NULL,
			created_at TEXT DEFAULT 'CURRENT_TIMESTAMP'
		);

		CREATE TABLE posts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			author_id INTEGER NOT NULL,
			title TEXT NOT NULL,
			FOREIGN KEY (author_id) REFERENCES users(id) ON DELETE CASCADE
		);

		CREATE INDEX idx_posts_author ON posts(author_id);
	`

	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV1,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("Sync V1 failed: %v\nLogs:\n%s", err, logBuf.String())
	}

	// Verify tables and index exist
	var tblCount int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name IN ('users', 'posts');").Scan(&tblCount)
	if err != nil || tblCount != 2 {
		t.Fatalf("expected 2 tables, got count %d, err: %v", tblCount, err)
	}

	var idxCount int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='idx_posts_author';").Scan(&idxCount)
	if err != nil || idxCount != 1 {
		t.Fatalf("expected index idx_posts_author, got count %d, err: %v", idxCount, err)
	}

	// Phase 2: Idempotency (warm boot)
	plan, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV1,
	})
	if err != nil {
		t.Fatalf("PlanDiff warm boot failed: %v", err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("expected 0 steps on warm boot, got %d steps: %v", len(plan.Steps), plan.Steps)
	}

	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV1,
	})
	if err != nil {
		t.Fatalf("Sync warm boot failed: %v", err)
	}

	// Phase 3: Non-destructive Column Addition (native ALTER TABLE ADD COLUMN)
	schemaV2 := `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT NOT NULL,
			created_at TEXT DEFAULT 'CURRENT_TIMESTAMP',
			status TEXT DEFAULT 'active'
		);

		CREATE TABLE posts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			author_id INTEGER NOT NULL,
			title TEXT NOT NULL,
			FOREIGN KEY (author_id) REFERENCES users(id) ON DELETE CASCADE
		);

		CREATE INDEX idx_posts_author ON posts(author_id);
	`

	planV2, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV2,
	})
	if err != nil {
		t.Fatalf("PlanDiff V2 failed: %v", err)
	}
	if len(planV2.Steps) != 1 || planV2.Steps[0].Type != grizzle.ChangeAddColumn {
		t.Fatalf("expected 1 ADD_COLUMN step, got: %+v", planV2.Steps)
	}

	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV2,
	})
	if err != nil {
		t.Fatalf("Sync V2 failed: %v", err)
	}

	// Phase 4: Insert row data, then perform Table Rebuild (dropping created_at column)
	_, err = db.ExecContext(ctx, "INSERT INTO users (id, email, status) VALUES (1, 'alice@example.com', 'active');")
	if err != nil {
		t.Fatalf("failed inserting test user: %v", err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO posts (id, author_id, title) VALUES (10, 1, 'First Post');")
	if err != nil {
		t.Fatalf("failed inserting test post: %v", err)
	}

	// Schema V3: Dropped column 'created_at' from users
	schemaV3 := `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT NOT NULL,
			status TEXT DEFAULT 'active'
		);

		CREATE TABLE posts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			author_id INTEGER NOT NULL,
			title TEXT NOT NULL,
			FOREIGN KEY (author_id) REFERENCES users(id) ON DELETE CASCADE
		);

		CREATE INDEX idx_posts_author ON posts(author_id);
	`

	// Attempting without AllowDrop should fail
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV3,
		AllowDrop: false,
	})
	if err == nil {
		t.Fatalf("expected DestructiveViolationError when dropping column with AllowDrop=false")
	}
	if _, ok := errors.AsType[*grizzle.DestructiveViolationError](err); !ok {
		t.Fatalf("expected DestructiveViolationError, got: %T: %v", err, err)
	}

	// With AllowDropColumn: true and AcceptHazards, 12-step rebuild succeeds and preserves rows!
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:         grizzle.DialectSQLite,
		SchemaSQL:       schemaV3,
		AllowDropColumn: new(true),
		AcceptHazards:   []grizzle.HazardCode{grizzle.HazardDropColumn},
	})
	if err != nil {
		t.Fatalf("Sync V3 rebuild failed: %v", err)
	}

	// Verify data preservation
	var email, status string
	err = db.QueryRowContext(ctx, "SELECT email, status FROM users WHERE id = 1;").Scan(&email, &status)
	if err != nil {
		t.Fatalf("failed querying user after rebuild: %v", err)
	}
	if email != "alice@example.com" || status != "active" {
		t.Fatalf("unexpected user row after rebuild: email=%q, status=%q", email, status)
	}

	var title string
	err = db.QueryRowContext(ctx, "SELECT title FROM posts WHERE id = 10;").Scan(&title)
	if err != nil {
		t.Fatalf("failed querying post after rebuild: %v", err)
	}
	if title != "First Post" {
		t.Fatalf("unexpected post row after rebuild: title=%q", title)
	}
}

func TestSQLite_AutoDialectDetection(t *testing.T) {
	db := getSQLiteDB(t)
	ctx := t.Context()

	schema := `CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL);`

	// Dialect is empty (DialectAuto), should automatically detect SQLite and succeed
	err := grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schema,
	})
	if err != nil {
		t.Fatalf("Sync with DialectAuto failed: %v", err)
	}

	plan, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		SchemaSQL: schema,
	})
	if err != nil {
		t.Fatalf("PlanDiff with DialectAuto failed: %v", err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("expected 0 steps, got %d", len(plan.Steps))
	}
}

func TestSQLite_FineGrainedSafetyPolicy(t *testing.T) {
	db := getSQLiteDB(t)
	ctx := t.Context()

	v1 := `
		CREATE TABLE keep_me (id INTEGER PRIMARY KEY);
		CREATE TABLE drop_me (id INTEGER PRIMARY KEY);
	`
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: v1}); err != nil {
		t.Fatal(err)
	}

	v2 := `CREATE TABLE keep_me (id INTEGER PRIMARY KEY);`

	// 1. Blocked when AllowDrop = false
	err := grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: v2,
		AllowDrop: false,
	})
	if err == nil {
		t.Fatal("expected error dropping table with AllowDrop=false")
	}

	// 2. Allowed when AllowDropTable = true and AcceptHazards has HazardDropTable
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL:      v2,
		AllowDrop:      false,
		AllowDropTable: new(true),
		AcceptHazards:  []grizzle.HazardCode{grizzle.HazardDropTable},
	})
	if err != nil {
		t.Fatalf("expected success with AllowDropTable=true, got: %v", err)
	}

	// Verify table was dropped
	var count int
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='drop_me';").Scan(&count)
	if count != 0 {
		t.Fatalf("table drop_me should have been dropped, count=%d", count)
	}
}

func TestSQLite_PlanVisualizer(t *testing.T) {
	db := getSQLiteDB(t)
	ctx := t.Context()

	schema := `
		CREATE TABLE inventory (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sku TEXT NOT NULL,
			quantity INTEGER DEFAULT 0
		);
		CREATE INDEX idx_sku ON inventory(sku);
	`

	plan, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		SchemaSQL: schema,
	})
	if err != nil {
		t.Fatal(err)
	}

	adds, alters, drops, blocked := plan.Summary()
	if adds != 2 || alters != 0 || drops != 0 || blocked != 0 {
		t.Fatalf("unexpected summary: adds=%d, alters=%d, drops=%d, blocked=%d", adds, alters, drops, blocked)
	}

	formatted := plan.String()
	if formatted == "" {
		t.Fatal("expected non-empty plan formatting")
	}
}

func TestSQLite_RebuildPreservesViewsAndTriggers(t *testing.T) {
	db := getSQLiteDB(t)
	db.SetMaxOpenConns(1)
	ctx := t.Context()

	// Initial schema with products and audit_log
	schemaV1 := `
		CREATE TABLE products (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			description TEXT
		);
		CREATE TABLE audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			product_id INTEGER,
			action TEXT
		);
	`
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV1,
	}); err != nil {
		t.Fatalf("Sync V1 failed: %v", err)
	}

	// Create view and trigger directly on live database
	viewSQL := `CREATE VIEW product_names AS SELECT id, name FROM products;`
	if _, err := db.ExecContext(ctx, viewSQL); err != nil {
		t.Fatalf("failed to create view: %v", err)
	}

	triggerSQL := `CREATE TRIGGER trg_product_audit AFTER UPDATE ON products
BEGIN
	INSERT INTO audit_log (product_id, action) VALUES (NEW.id, 'updated');
END;`
	if _, err := db.ExecContext(ctx, triggerSQL); err != nil {
		t.Fatalf("failed to create trigger: %v", err)
	}

	// Insert test data
	if _, err := db.ExecContext(ctx, "INSERT INTO products (name, description) VALUES ('gadget', 'great device');"); err != nil {
		t.Fatalf("failed to insert product: %v", err)
	}

	// Schema V2: Drop column 'description' from products, requiring a table rebuild
	schemaV2 := `
		CREATE TABLE products (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL
		);
		CREATE TABLE audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			product_id INTEGER,
			action TEXT
		);
	`

	// Rebuild products table with AllowDropColumn: true
	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:         grizzle.DialectSQLite,
		SchemaSQL:       schemaV2,
		AllowDropColumn: new(true),
		AcceptHazards:   []grizzle.HazardCode{grizzle.HazardDropColumn},
	})
	if err != nil {
		t.Fatalf("Sync V2 rebuild failed: %v", err)
	}

	// 1. Verify view product_names is still valid and returns product data
	var vID int
	var vName string
	err = db.QueryRowContext(ctx, "SELECT id, name FROM product_names WHERE id = 1;").Scan(&vID, &vName)
	if err != nil {
		t.Fatalf("querying preserved view failed: %v", err)
	}
	if vName != "gadget" {
		t.Fatalf("view returned unexpected name %q, want 'gadget'", vName)
	}

	// 2. Verify trigger trg_product_audit is preserved and fires on UPDATE
	if _, err := db.ExecContext(ctx, "UPDATE products SET name = 'gadget-v2' WHERE id = 1;"); err != nil {
		t.Fatalf("updating product failed: %v", err)
	}

	var auditAction string
	var auditProductID int
	err = db.QueryRowContext(ctx, "SELECT product_id, action FROM audit_log WHERE product_id = 1;").Scan(&auditProductID, &auditAction)
	if err != nil {
		t.Fatalf("querying audit_log after trigger execution failed: %v", err)
	}
	if auditAction != "updated" {
		t.Fatalf("expected audit action 'updated', got %q", auditAction)
	}

	// 3. Verify warm boot produces 0 diffs
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV2,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("expected 0 diff steps after rebuild, got %d: %+v", len(p.Steps), p.Steps)
	}
}

func TestSQLite_RebuildBatchKeysetCopy(t *testing.T) {
	db := getSQLiteDB(t)
	db.SetMaxOpenConns(1)
	ctx := t.Context()

	// Initial schema with extra column
	schemaV1 := `
		CREATE TABLE items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			val INTEGER NOT NULL,
			extra TEXT
		);
	`
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV1,
	}); err != nil {
		t.Fatalf("Sync V1 failed: %v", err)
	}

	// Insert 50 rows
	for i := 1; i <= 50; i++ {
		_, err := db.ExecContext(ctx, "INSERT INTO items (val, extra) VALUES (?, ?);", i, "data")
		if err != nil {
			t.Fatalf("failed inserting item %d: %v", i, err)
		}
	}

	// Schema V2: Drop column 'extra', requiring rebuild
	schemaV2 := `
		CREATE TABLE items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			val INTEGER NOT NULL
		);
	`

	// Sync with small threshold (10) and batch size (5) to force multiple chunked copies
	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:                grizzle.DialectSQLite,
		SchemaSQL:              schemaV2,
		AllowDropColumn:        new(true),
		AcceptHazards:          []grizzle.HazardCode{grizzle.HazardDropColumn},
		SQLiteRebuildThreshold: 10,
		SQLiteRebuildBatchSize: 5,
	})
	if err != nil {
		t.Fatalf("Sync with chunked keyset batch copy failed: %v", err)
	}

	// Verify all 50 rows exist and sum of val is 50*51/2 = 1275
	var count, sumVal int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*), SUM(val) FROM items;").Scan(&count, &sumVal)
	if err != nil {
		t.Fatalf("querying items after batch rebuild failed: %v", err)
	}
	if count != 50 {
		t.Fatalf("expected 50 rows after rebuild, got %d", count)
	}
	if sumVal != 1275 {
		t.Fatalf("expected sum(val) 1275, got %d", sumVal)
	}
}

func TestSQLite_KeysetBatchCopy_MidChunkFailureRollback(t *testing.T) {
	db := getSQLiteDB(t)
	db.SetMaxOpenConns(1)
	ctx := t.Context()

	// Initial schema: nullable column
	schemaV1 := `
		CREATE TABLE items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			val INTEGER,
			extra TEXT
		);
	`
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV1,
	}); err != nil {
		t.Fatalf("Sync V1 failed: %v", err)
	}

	// Insert 30 rows. Rows 1..10 have non-null val.
	// Row 15 has NULL val!
	// Rows 16..30 have non-null val.
	for i := 1; i <= 30; i++ {
		var val any = i
		if i == 15 {
			val = nil
		}
		_, err := db.ExecContext(ctx, "INSERT INTO items (val, extra) VALUES (?, ?);", val, "data")
		if err != nil {
			t.Fatalf("failed inserting item %d: %v", i, err)
		}
	}

	// Schema V2: Drop column 'extra', and make 'val' NOT NULL.
	// This triggers a table rebuild where chunk 1 (1..5) succeeds, chunk 2 (6..10) succeeds,
	// but chunk 3 (11..15) encounters row 15 with NULL in NOT NULL column and fails!
	schemaV2 := `
		CREATE TABLE items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			val INTEGER NOT NULL
		);
	`

	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:                grizzle.DialectSQLite,
		SchemaSQL:              schemaV2,
		AllowDropColumn:        new(true),
		AcceptHazards:          []grizzle.HazardCode{grizzle.HazardDropColumn, grizzle.HazardNotNullNoDefault},
		SQLiteRebuildThreshold: 10,
		SQLiteRebuildBatchSize: 5,
	})
	if err == nil {
		t.Fatalf("expected Sync to fail due to NOT NULL violation during chunked copy, but got nil")
	}
	if !strings.Contains(err.Error(), "NOT NULL constraint failed") && !strings.Contains(err.Error(), "chunked keyset copy failed") {
		t.Fatalf("expected error to mention chunked keyset copy failure or constraint violation, got: %v", err)
	}

	// Assert original table is completely intact!
	var count int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM items;").Scan(&count)
	if err != nil {
		t.Fatalf("querying original items table failed: %v", err)
	}
	if count != 30 {
		t.Fatalf("expected all 30 original rows intact, got %d", count)
	}

	// Assert original column 'extra' still exists and row 15 still has NULL
	var row15Extra string
	var row15Val sql.NullInt64
	err = db.QueryRowContext(ctx, "SELECT val, extra FROM items WHERE id = 15;").Scan(&row15Val, &row15Extra)
	if err != nil {
		t.Fatalf("failed querying row 15 from original table: %v", err)
	}
	if row15Val.Valid {
		t.Fatalf("expected row 15 val to remain NULL, got %d", row15Val.Int64)
	}
	if row15Extra != "data" {
		t.Fatalf("expected row 15 extra column to be intact, got %s", row15Extra)
	}

	// Verify temp table _grizzle_new_items was rolled back and does not exist
	var tempExists int
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_grizzle_new_items';").Scan(&tempExists)
	if tempExists != 0 {
		t.Fatalf("expected temporary table _grizzle_new_items to be cleaned up after rollback")
	}
}

func TestSQLite_RebuildSavepointFKViolationRollback(t *testing.T) {
	db := getSQLiteDB(t)
	db.SetMaxOpenConns(1)
	ctx := t.Context()

	schemaV1 := `
		CREATE TABLE parents (
			id INTEGER PRIMARY KEY
		);
		CREATE TABLE children (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER NOT NULL REFERENCES parents(id),
			extra TEXT
		);
	`
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV1,
	}); err != nil {
		t.Fatalf("Sync V1 failed: %v", err)
	}

	// Insert valid parent and valid child
	if _, err := db.ExecContext(ctx, "INSERT INTO parents (id) VALUES (1);"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO children (id, parent_id, extra) VALUES (10, 1, 'ok');"); err != nil {
		t.Fatal(err)
	}

	// Temporarily disable FKs to insert an orphaned child that violates FK constraint
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF;"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO children (id, parent_id, extra) VALUES (20, 999, 'orphan');"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON;"); err != nil {
		t.Fatal(err)
	}

	// Schema V2: Drop extra from children, triggering a rebuild of children
	schemaV2 := `
		CREATE TABLE parents (
			id INTEGER PRIMARY KEY
		);
		CREATE TABLE children (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER NOT NULL REFERENCES parents(id)
		);
	`

	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:         grizzle.DialectSQLite,
		SchemaSQL:       schemaV2,
		AllowDropColumn: new(true),
		AcceptHazards:   []grizzle.HazardCode{grizzle.HazardDropColumn},
	})
	if err == nil {
		t.Fatalf("expected error due to foreign key violation, got nil")
	}

	if !errors.Is(err, grizzle.ErrExecutionFailed) {
		t.Logf("got error: %v", err)
	}

	// Verify that table children was not modified to V2 and extra column still exists
	var count int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM children WHERE extra = 'orphan';").Scan(&count)
	if err != nil {
		t.Fatalf("table children rollback failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected orphaned row with extra='orphan' still present, got count %d", count)
	}
}

func TestSQLite_KeysetBatchCopy_LargeVolume_Checksum(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large volume sqlite test in short mode")
	}

	db := getSQLiteDB(t)
	db.SetMaxOpenConns(1)
	ctx := t.Context()

	schemaV1 := `
		CREATE TABLE logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			val INTEGER NOT NULL,
			payload TEXT NOT NULL,
			temp_tag TEXT
		);
	`
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV1,
	}); err != nil {
		t.Fatalf("Sync V1 failed: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO logs (val, payload, temp_tag) VALUES (?, ?, ?);")
	if err != nil {
		t.Fatal(err)
	}
	totalRows := 20000
	for i := 1; i <= totalRows; i++ {
		if _, err := stmt.ExecContext(ctx, i, fmt.Sprintf("payload_%d", i), "tag"); err != nil {
			t.Fatal(err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var preCount, preSumVal int64
	err = db.QueryRowContext(ctx, "SELECT COUNT(*), SUM(val) FROM logs;").Scan(&preCount, &preSumVal)
	if err != nil {
		t.Fatal(err)
	}

	// Schema V2: Drop column 'temp_tag' requiring rebuild with chunked copying
	schemaV2 := `
		CREATE TABLE logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			val INTEGER NOT NULL,
			payload TEXT NOT NULL
		);
	`

	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:                grizzle.DialectSQLite,
		SchemaSQL:              schemaV2,
		AllowDropColumn:        new(true),
		AcceptHazards:          []grizzle.HazardCode{grizzle.HazardDropColumn},
		SQLiteRebuildThreshold: 5000,
		SQLiteRebuildBatchSize: 2000,
	})
	if err != nil {
		t.Fatalf("Sync V2 chunked rebuild failed: %v", err)
	}

	var postCount, postSumVal int64
	err = db.QueryRowContext(ctx, "SELECT COUNT(*), SUM(val) FROM logs;").Scan(&postCount, &postSumVal)
	if err != nil {
		t.Fatal(err)
	}
	if postCount != preCount {
		t.Errorf("row count mismatch after rebuild: got %d, want %d", postCount, preCount)
	}
	if postSumVal != preSumVal {
		t.Errorf("sum(val) checksum mismatch after rebuild: got %d, want %d", postSumVal, preSumVal)
	}
}

func TestSQLite_RebuildPreservesAutoincrementSemantics(t *testing.T) {
	db := getSQLiteDB(t)
	ctx := t.Context()

	// Plain INTEGER PRIMARY KEY (no AUTOINCREMENT).
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT);`,
	}); err != nil {
		t.Fatalf("sync v1: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO items (id, name) VALUES (1, 'a'), (5, 'b');`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Force rebuild via nullability change of a non-PK column.
	allowCol := true
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:         grizzle.DialectSQLite,
		SchemaSQL:       `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL);`,
		AllowDropColumn: &allowCol,
		AcceptHazards:   []grizzle.HazardCode{grizzle.HazardDropColumn},
	}); err != nil {
		t.Fatalf("sync rebuild: %v", err)
	}

	var ddl string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='items';`).Scan(&ddl); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT") {
		t.Fatalf("rebuild must not introduce AUTOINCREMENT; ddl=%s", ddl)
	}

	// Without AUTOINCREMENT, SQLite chooses max(remaining rowid)+1 after a
	// delete of the high-water mark — not the historical max tracked by
	// sqlite_sequence. After deleting id=5 (leaving id=1), the next insert
	// must get 2. AUTOINCREMENT would have advanced to 6.
	if _, err := db.ExecContext(ctx, `DELETE FROM items WHERE id = 5;`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO items (name) VALUES ('c');`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var newID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM items WHERE name='c';`).Scan(&newID); err != nil {
		t.Fatalf("select: %v", err)
	}
	if newID != 2 {
		t.Fatalf("expected rowid reuse semantics without AUTOINCREMENT (id=2), got %d", newID)
	}
}

func TestSQLite_RebuildPreservesAutoincrementFlag(t *testing.T) {
	db := getSQLiteDB(t)
	ctx := t.Context()

	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: `CREATE TABLE seq_items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT);`,
	}); err != nil {
		t.Fatalf("sync v1: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO seq_items (id, name) VALUES (1, 'a'), (5, 'b');`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	allowCol := true
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:         grizzle.DialectSQLite,
		SchemaSQL:       `CREATE TABLE seq_items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL);`,
		AllowDropColumn: &allowCol,
		AcceptHazards:   []grizzle.HazardCode{grizzle.HazardDropColumn},
	}); err != nil {
		t.Fatalf("sync rebuild: %v", err)
	}

	var ddl string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='seq_items';`).Scan(&ddl); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if !strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT") {
		t.Fatalf("rebuild must preserve AUTOINCREMENT; ddl=%s", ddl)
	}

	// After copying explicit ids 1 and 5 into an AUTOINCREMENT table,
	// sqlite_sequence high-water is 5. Deleting 5 then inserting must
	// yield 6 — not reuse 2 the way plain INTEGER PRIMARY KEY would.
	if _, err := db.ExecContext(ctx, `DELETE FROM seq_items WHERE id = 5;`); err != nil {
		t.Fatalf("delete max: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO seq_items (name) VALUES ('c');`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var newID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM seq_items WHERE name='c';`).Scan(&newID); err != nil {
		t.Fatalf("select: %v", err)
	}
	if newID != 6 {
		t.Fatalf("expected AUTOINCREMENT high-water continuation (id=6), got %d", newID)
	}
}

func TestSQLite_WithoutRowID_RebuildAboveThreshold(t *testing.T) {
	db := getSQLiteDB(t)
	ctx := t.Context()

	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: `CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID;`,
	}); err != nil {
		t.Fatalf("sync v1: %v", err)
	}

	const n = 50
	for i := range n {
		if _, err := db.ExecContext(ctx, `INSERT INTO kv (k, v) VALUES (?, ?);`, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	allowCol := true
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:                grizzle.DialectSQLite,
		SchemaSQL:              `CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;`,
		AllowDropColumn:        &allowCol,
		AcceptHazards:          []grizzle.HazardCode{grizzle.HazardDropColumn},
		SQLiteRebuildThreshold: 10,
		SQLiteRebuildBatchSize: 5,
	}); err != nil {
		t.Fatalf("rebuild WITHOUT ROWID: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kv;`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != n {
		t.Fatalf("row count after WITHOUT ROWID rebuild: got %d want %d", count, n)
	}
}
