package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

func ExampleSync() {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL
);`

	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schema,
	})
	if err != nil {
		log.Fatalf("sync failed: %v", err)
	}

	var count int
	_ = db.QueryRow("SELECT count(*) FROM users;").Scan(&count)
	fmt.Printf("Users table exists with row count: %d\n", count)

	// Output:
	// Users table exists with row count: 0
}

func ExamplePlanDiff() {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `
CREATE TABLE products (
    id INTEGER PRIMARY KEY,
    sku TEXT NOT NULL UNIQUE,
    price INTEGER NOT NULL DEFAULT 0
);`

	plan, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schema,
	})
	if err != nil {
		log.Fatalf("plan diff failed: %v", err)
	}

	fmt.Printf("Plan steps to execute: %d\n", len(plan.Steps))

	// Output:
	// Plan steps to execute: 1
}
