// Package main demonstrates zero-downtime expand/contract migrations with
// Grizzle: a column rename executed in two separately-approved stages with a
// library-side backfill between them.
//
//	Phase 1 (expand):   add users.display_name alongside users.full_name,
//	                    nullable, while the application keeps both alive.
//	Phase 2 (backfill): copy existing values outside the DDL lock window
//	                    (library-only: Options.Backfill / WithBackfill).
//	Phase 3 (contract): a second plan drops users.full_name once the
//	                    application only reads display_name.
//
// The CLI exposes --expand-contract and --rename for planning the same flow;
// backfill remains library-only.
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

//go:embed schema.sql
var schemaSQL string

// bootstrapSQL creates the pre-migration (v1) shape if it does not exist yet.
const bootstrapSQL = `
CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL UNIQUE,
    full_name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}

	targetSchema := os.Getenv("PG_SCHEMA")
	renames := map[string]string{"users.full_name": "display_name"}

	// Bootstrap the v1 shape (no-op when the table already exists).
	bootCtx, bootCancel := context.WithTimeout(ctx, time.Minute)
	if err := bootstrap(bootCtx, db, targetSchema); err != nil {
		bootCancel()
		log.Fatalf("bootstrap failed: %v", err)
	}
	bootCancel()

	// ---- Phase 1+2: EXPAND — add display_name alongside full_name and
	// backfill values outside the DDL lock window. full_name stays intact and
	// the application keeps working throughout.
	expandCtx, expandCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer expandCancel()
	err = grizzle.Sync(expandCtx, db, grizzle.Options{
		TargetSchema:   targetSchema,
		SchemaSQL:      schemaSQL,
		Renames:        renames,
		ExpandContract: true, // Experimental: staged expand-and-contract
		Backfill: func(_ context.Context, tx *sql.Tx, table, oldCol, newCol string) error {
			// Library-only hook: run in batches inside a dedicated
			// transaction, outside the DDL lock window. The hook receives the
			// bare table name, so qualify it with the target schema.
			tbl := fmt.Sprintf("%q", table)
			if targetSchema != "" {
				tbl = fmt.Sprintf("%q.%q", targetSchema, table)
			}
			//nolint:gosec // G701: identifiers come from Grizzle's rename map / env-scoped example, quoted with %q
			_, err := tx.Exec(fmt.Sprintf(
				"UPDATE %s SET %q = %q WHERE %q IS NULL",
				tbl, newCol, oldCol, newCol))
			return err
		},
	})
	if err != nil {
		log.Fatalf("expand phase failed: %v", err)
	}
	log.Println("expand phase complete: display_name added and backfilled; full_name still live")

	// ---- Phase 3: CONTRACT — once the application only reads display_name,
	// a second, separately-approved plan drops full_name. Destructive, so it
	// requires AllowDropColumn and explicit hazard acceptance.
	contractCtx, contractCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer contractCancel()
	err = grizzle.Sync(contractCtx, db, grizzle.Options{
		TargetSchema:    targetSchema,
		SchemaSQL:       schemaSQL,
		Renames:         renames,
		AllowDropColumn: ptr(true),
		AcceptHazards:   []grizzle.HazardCode{grizzle.HazardDropColumn},
	})
	if err != nil {
		log.Fatalf("contract phase failed: %v", err)
	}
	log.Println("contract phase complete: full_name dropped; rename finished with zero downtime")
}

// bootstrap applies the v1 DDL outside Grizzle so the example is runnable
// against an empty database. In production the v1 shape already exists.
func bootstrap(ctx context.Context, db *sql.DB, targetSchema string) error {
	exec := func(stmt string) error {
		//nolint:gosec // G701: statements are static bootstrap DDL or env-derived identifiers in a local example
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
		return nil
	}
	if targetSchema != "" {
		if err := exec(fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %q;", targetSchema)); err != nil {
			return err
		}
		//nolint:gosec // G701: targetSchema is a local example env identifier, quoted with %q
		if _, err := db.ExecContext(ctx, fmt.Sprintf("SET search_path TO %q", targetSchema)); err != nil {
			return err
		}
	}
	return exec(bootstrapSQL)
}

func ptr[T any](v T) *T { return &v }
