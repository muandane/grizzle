// Package main demonstrates RolesSQL side-channel privilege sync: schema
// objects live in schema.sql; CREATE ROLE / GRANT statements live in roles.sql.
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

//go:embed schema.sql
var schemaSQL string

//go:embed roles.sql
var rolesSQL string

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

	syncCtx, syncCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer syncCancel()

	if err := db.PingContext(syncCtx); err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}

	var currentDatabase string
	if err := db.QueryRowContext(syncCtx, `SELECT current_database()`).Scan(&currentDatabase); err != nil {
		log.Fatalf("failed to read current database: %v", err)
	}

	targetSchema := os.Getenv("PG_SCHEMA")
	err = grizzle.Sync(syncCtx, db, grizzle.Options{
		TargetSchema: targetSchema,
		SchemaSQL:    schemaSQL,
		RolesSQL:     rolesForTarget(rolesSQL, targetSchema, currentDatabase),
		AllowDrop:    false,
	})
	if err != nil {
		log.Fatalf("database schema sync failed: %v", err)
	}

	log.Println("Schema and roles are up-to-date.")
}

// rolesForTarget retargets the schema- and database-level grants in roles.sql
// (written with the CLI defaults "public" and "app") to the connected target.
// Object names in roles.sql are unqualified and already bind to the target
// schema; grizzle rejects SCHEMA/DATABASE grants outside the current scope,
// so those two must follow it.
func rolesForTarget(roles, targetSchema, currentDatabase string) string {
	out := roles
	if targetSchema != "" && targetSchema != "public" {
		out = strings.Replace(out, "ON SCHEMA public ", "ON SCHEMA "+quoteIdent(targetSchema)+" ", 1)
	}
	if currentDatabase != "" && currentDatabase != "app" {
		out = strings.Replace(out, "ON DATABASE app ", "ON DATABASE "+quoteIdent(currentDatabase)+" ", 1)
	}
	return out
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
