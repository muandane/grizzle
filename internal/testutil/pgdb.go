// Package testutil provides shared test infrastructure for integration tests.
package testutil

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const defaultDSN = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"

// PostgresDSN resolves the PostgreSQL connection string from the environment.
// It checks DATABASE_URL first, then POSTGRES_DSN, then falls back to a
// localhost default.
func PostgresDSN() string {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn
	}
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return defaultDSN
}

// TestDatabase opens a PostgreSQL connection for integration tests.
// It resolves the DSN from the environment via [PostgresDSN], pings the
// database with a 5-second timeout, and registers a cleanup function to
// close the connection when the test finishes.
//
// The caller decides whether to skip or fail when PostgreSQL is unavailable:
//
//	db := testutil.TestDatabase(t) // fatals if unavailable
//
// For tests that should skip instead, check [PostgresDSN] and ping manually.
func TestDatabase(t *testing.T) *sql.DB {
	t.Helper()

	dsn := PostgresDSN()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("connect to database at %s: %v", dsn, err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}
