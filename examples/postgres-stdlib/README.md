# Using Grizzle with the Go standard library

## Overview

Minimal in-process PostgreSQL automigration using only `database/sql` and the `pgx` stdlib driver. `schema.sql` is the single source of truth, embedded at build time with `//go:embed` and synced on application boot.

## Usage pattern

1. Read `DATABASE_URL` (fallback: `postgres://postgres:password@localhost:5432/myapp?sslmode=disable`) and optional `PG_SCHEMA` for schema scoping.
2. Bound the startup migration with a `context.WithTimeout` so the server's request context never inherits the migration deadline.
3. Call `grizzle.Sync(ctx, db, grizzle.Options{TargetSchema, SchemaSQL})` — advisory locking, shadow compilation, and hazard gates all run in-process. Drops stay disabled (`AllowDrop: false`) for strict production safety.

## Running

```bash
# With devenv (starts a local PostgreSQL on 5432)
devenv up
go run .                       # or: DATABASE_URL=... PG_SCHEMA=my_schema go run .

# Integration test (drops a scratch schema afterwards)
go test -tags integration ./...
```

See [examples/expand-contract](../expand-contract) for zero-downtime renames, and [docs/SAFETY.md](../../docs/SAFETY.md) for the hazard-gate behavior this example relies on.