# Using Grizzle with pgxpool

This example demonstrates how to run Grizzle automigrations in applications that use `github.com/jackc/pgx/v5/pgxpool` for native connection pooling.

## Overview

High-throughput Go services often use `pgxpool.Pool` directly rather than `database/sql` to access pgx-specific features such as binary protocol optimizations and custom data types.

Grizzle operates on `*sql.DB`. The `github.com/jackc/pgx/v5/stdlib` package provides `stdlib.OpenDBFromPool(pool)`:
* It adapts an existing `*pgxpool.Pool` into a standard `*sql.DB` interface.
* It reuses connections directly from the `pgxpool.Pool` without opening a secondary connection pool.
* Closing the resulting `*sql.DB` does not close the underlying `pgxpool.Pool`.

## Usage pattern

```go
package main

import (
	"context"
	_ "embed"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://user:pass@localhost:5432/app?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// 1. Adapt pool to *sql.DB for boot migration
	sqlDB := stdlib.OpenDBFromPool(pool)
	if err := grizzle.Sync(ctx, sqlDB, grizzle.Options{
		SchemaSQL: schemaSQL,
	}); err != nil {
		log.Fatalf("migration failed: %v", err)
	}

	// 2. Execute application queries via native pgxpool
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM metrics").Scan(&count); err != nil {
		log.Fatal(err)
	}
}
```

## Running the example

Run integration tests against PostgreSQL:

```bash
DATABASE_URL="postgres://postgres:password@localhost:5432/grizzle_test?sslmode=disable" \
go test -tags integration -v ./examples/postgres-pgxpool
```
