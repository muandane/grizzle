//go:build integration

package main

import (
	"database/sql"
	"os"
	"testing"
)

func TestExample_PostgresPGXPool(t *testing.T) {
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

	schema := "test_example_pgxpool"
	_, _ = db.Exec("CREATE SCHEMA IF NOT EXISTS " + schema + ";")
	defer func() { _, _ = db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE;") }()

	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("PG_SCHEMA", schema)
	main()
}
