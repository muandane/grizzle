package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	ctx := context.Background()

	// Pure Go embedded SQLite (no Cgo required)
	db, err := sql.Open("sqlite", "file:app.db")
	if err != nil {
		log.Fatalf("failed to open sqlite database: %v", err)
	}
	defer func() { _ = db.Close() }()

	log.Println("Synchronizing embedded SQLite schema with Grizzle...")

	// Grizzle automatically detects SQLite dialect from the driver
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaSQL,
	})
	if err != nil {
		log.Fatalf("sqlite schema sync failed: %v", err)
	}

	log.Println("SQLite database is ready! Starting embedded service...")
}
