# Using Grizzle with embedded SQLite

## Overview

In-process declarative migrations for embedded SQLite with zero Cgo (pure-Go `modernc.org/sqlite`). The same `schema.sql` single-source-of-truth workflow as the PostgreSQL example, but the shadow compile runs in an in-memory database — fully offline.

## Usage pattern

1. Open the database with `sql.Open("sqlite", dsn)`; a `sqlite://` or `.db` DSN selects the SQLite dialect automatically (or set `Dialect: grizzle.DialectSQLite` explicitly).
2. Call `grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schemaSQL})` — no network, no locks beyond SQLite's own file locking.
3. Column type changes and drops route through Grizzle's SQLite 12-step table rebuild engine (chunked keyset copy, savepoint isolation, foreign-key validation, trigger/view preservation).

## SQLite scope notes

- Multi-schema configurations are rejected (`ErrUnsupportedMultiSchema`) — SQLite operates on one attached database per connection.
- CHECK constraints are not introspected or diffed; rebuilds rewrite tables from the managed IR (see SPEC §3).

## Running

```bash
go run .                # uses ./app.db by default
go test -tags integration ./...
```