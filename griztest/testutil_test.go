package griztest_test

import (
	"database/sql"
	"testing"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/griztest"
	_ "modernc.org/sqlite"
)

func TestGriztest_MustSyncAndMustPlan(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	initialSchema := `
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL
);`

	// 1. MustSync initializes the schema
	griztest.MustSync(t, db, initialSchema, grizzle.WithDialect(grizzle.DialectSQLite))

	// Verify table exists
	var count int
	err = db.QueryRow("SELECT count(*) FROM users;").Scan(&count)
	if err != nil {
		t.Fatalf("querying table users: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows, got %d", count)
	}

	// 2. MustPlan computes the diff
	nextSchema := `
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT
);`

	p := griztest.MustPlan(t, db, nextSchema, grizzle.WithDialect(grizzle.DialectSQLite))
	if p == nil {
		t.Fatalf("expected non-nil plan")
	}
	if p.Additions() != 1 {
		t.Errorf("expected 1 addition (new column), got %d", p.Additions())
	}

	// 3. Reset cleanly applies updated schema with AllowDrop: true
	griztest.Reset(t, db, nextSchema, grizzle.WithDialect(grizzle.DialectSQLite))

	// Verify new column exists
	_, err = db.Exec("INSERT INTO users (id, name, email) VALUES (1, 'Alice', 'alice@example.com');")
	if err != nil {
		t.Fatalf("inserting into updated schema: %v", err)
	}
}
