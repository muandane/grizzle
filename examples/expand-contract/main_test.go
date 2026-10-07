//go:build integration

package main

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestExample_ExpandContract(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		dsn = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("integration postgres not available: %v", err)
	}

	schema := "test_example_expand_contract"
	_, _ = db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE;")
	_, _ = db.Exec("CREATE SCHEMA " + schema + ";")
	defer func() { _, _ = db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE;") }()

	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("PG_SCHEMA", schema)
	main()

	// Verify the final shape: full_name gone, display_name populated.
	var colCount int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'full_name'`, schema).Scan(&colCount); err != nil {
		t.Fatalf("checking full_name: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("expected full_name to be dropped in contract phase")
	}

	var displayCount int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'display_name'`, schema).Scan(&displayCount); err != nil {
		t.Fatalf("checking display_name: %v", err)
	}
	if displayCount != 1 {
		t.Fatalf("expected display_name column to exist")
	}
}
