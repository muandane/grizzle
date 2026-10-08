// Package main demonstrates RolesSQL side-channel privilege sync: schema
// objects live in schema.sql; CREATE ROLE / GRANT statements live in roles.sql.
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

	targetSchema := os.Getenv("PG_SCHEMA")
	err = grizzle.Sync(syncCtx, db, grizzle.Options{
		TargetSchema: targetSchema,
		SchemaSQL:    schemaSQL,
		RolesSQL:     rolesSQL,
		AllowDrop:    false,
	})
	if err != nil {
		log.Fatalf("database schema sync failed: %v", err)
	}

	log.Println("Schema and roles are up-to-date.")
}
