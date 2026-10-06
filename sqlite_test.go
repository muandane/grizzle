package grizzle_test

import (
	"bytes"
	"database/sql"
	"errors"
	"log/slog"
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

