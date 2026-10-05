package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/yourorg/grizzle"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:password@localhost:5432/myapp?sslmode=disable"
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer db.Close()

	log.Println("Synchronizing database schema with Grizzle...")

	// Run in-process declarative migration on application boot
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaSQL,
		AllowDrop: false, // Strict safety in production
	})
	if err != nil {
		log.Fatalf("database schema sync failed: %v", err)
	}

	log.Println("Database schema is up-to-date! Starting HTTP service...")
	// Start your server here...
}
