// Package main demonstrates Grizzle's full managed surface: extensions,
// domains, tables with RLS + policies, COMMENT ON, functions, procedures,
// triggers, and (materialized) views — all declared in one schema.sql and
// synced in-process on boot.
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:password@localhost:5432/myapp?sslmode=disable"
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("invalid DSN: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Scope migration deadline to startup phase so server context does not inherit it
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer syncCancel()

	if err := db.PingContext(syncCtx); err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}

	targetSchema := os.Getenv("PG_SCHEMA")
	// Sync the full catalog: extension, domain, table, comments, RLS,
	// policy, functions, procedure, trigger, and views. Destructive steps
	// stay gated by default; accept them explicitly when you need them.
	err = grizzle.Sync(syncCtx, db, grizzle.Options{
		TargetSchema: targetSchema,
		SchemaSQL:    schemaSQL,
		AllowDrop:    false,
	})
	if err != nil {
		log.Fatalf("database schema sync failed: %v", err)
	}

	log.Println("Database schema is up-to-date! Starting HTTP service...")
	// Start your server here...
}
