# Grizzle

[![CI](https://github.com/muandane/grizzle/actions/workflows/ci.yml/badge.svg)](https://github.com/muandane/grizzle/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/muandane/grizzle.svg)](https://pkg.go.dev/github.com/muandane/grizzle)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Declarative schema migrations for Go.

Point Grizzle at your plain `schema.sql`, and it diffs and migrates your database on boot. No numbered migration files (`001_init.sql`), no external migration binaries inside your production containers.

- **Safe by default:** Destructive drops are blocked unless explicitly allowed.
- **Production-safe DDL:** Builds PostgreSQL indexes `CONCURRENTLY` and creates foreign keys as `NOT VALID` before validating.
- **Cluster-safe:** Uses PostgreSQL session advisory locks so multi-pod deployments won't collide.
- **Engine support:** PostgreSQL (14–18) and SQLite (3.35+, pure-Go or CGo).
- **Works with sqlc:** Share one `schema.sql` between sqlc and Grizzle.

---

## Quickstart

Embed your desired schema and run `Sync` on application startup:

```sql
-- schema.sql
CREATE TABLE users (
    id         BIGSERIAL PRIMARY KEY,
    email      VARCHAR(255) NOT NULL UNIQUE,
    full_name  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_users_created_at ON users (created_at);
```

```go
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	db, err := sql.Open("pgx", "postgres://postgres:secret@localhost:5432/myapp?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Bring the database in line with schema.sql
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		SchemaSQL:        schemaSQL,
		AllowDrop:        false, // Fails if the diff would drop columns or tables
		LockTimeout:      5 * time.Second,
		StatementTimeout: 2 * time.Minute,
	})
	if err != nil {
		log.Fatalf("migration failed: %v", err)
	}
}
```

> **Need approval gates?** Instead of auto-syncing on boot, use `grizzle.PlanDiff(...)` to generate a migration plan, review the SQL, then `grizzle.Apply(..., grizzle.ApplyOpts{ExpectedHash: plan.Hash()})` so apply aborts if the live database drifted.

---

## CLI

Use the CLI for CI drift checks, dry runs, or exporting plans to standard migration tools:

```bash
# Preview changes and generate a migration plan
grizzle plan --dsn "$DATABASE_URL" --schema schema.sql --out plan.json

# Apply a generated plan safely
grizzle apply --dsn "$DATABASE_URL" --plan plan.json

# Fail CI if live DB has drifted from schema.sql (exits with code 4)
grizzle check --dsn "$DATABASE_URL" --schema schema.sql

# Convert a plan to Goose migrations if you need an exit hatch
grizzle export --plan plan.json --format goose --out ./migrations
```

### Exit Codes

| Code | Meaning |
| --- | --- |
| `0` | Success / database is in sync |
| `1` | Connection or execution error |
| `2` | Blocked by an unaccepted hazard (e.g., unexpected DROP) |
| `3` | Plan hash mismatch (live database changed after plan was generated) |
| `4` | Schema drift detected (`grizzle check`) |

---

## Supported Features

### PostgreSQL (14–18)

* Tables, columns, enums, and domains.
* Non-blocking operations: indexes via `CONCURRENTLY`, foreign keys via `NOT VALID` + `VALIDATE CONSTRAINT`.
* Functions, procedures, triggers, views, materialized views, RLS policies, and extensions.
* Staged migrations: explicit column renames (`Options.Renames` / `--rename`) and expand/contract (`Options.ExpandContract` / `--expand-contract`, with optional `--backfill`).
* Optional role/privilege and publication/event-trigger SQL files (`--roles` / `RolesSQL`, `--catalog` / `CatalogSQL`).

### SQLite (3.35+)

* Tables, columns, foreign keys, CHECK constraints, and generated columns.
* Automatic 12-step table rebuilds for schema changes SQLite cannot execute in-place via `ALTER TABLE`.
* Supports both `modernc.org/sqlite` (pure Go) and `mattn/go-sqlite3` (CGo).

---

## Important Operational Notes

* **PgBouncer / Connection Poolers:** Grizzle relies on session-level advisory locks to prevent concurrent migrations. Connect directly to Postgres or use **Session Pooling**. Do not run migrations through **Transaction Pooling**.
* **Destructive Changes:** Dropping tables/columns requires `AllowDrop: true` (or `--allow-drop` in the CLI). See [docs/SAFETY.md](docs/SAFETY.md) for full hazard handling.

---

## Examples

* [postgres-stdlib](examples/postgres-stdlib) — `database/sql` + pgx boot sync
* [sqlite-embedded](examples/sqlite-embedded) — pure-Go SQLite setup
* [sqlc-workflow](examples/sqlc-workflow) — single source of truth: share `schema.sql` between sqlc and Grizzle
* [kubernetes-blueprint](examples/kubernetes-blueprint) — multi-replica startup coordination
* [expand-contract](examples/expand-contract) — expand / backfill / contract column rename
* [roles-grants](examples/roles-grants) / [catalog-sync](examples/catalog-sync) — roles and publications via optional SQL files
* [full-catalog](examples/full-catalog) — extensions, RLS, functions, triggers, and views in one `schema.sql`

## Documentation

* [SPEC](docs/SPEC.md) — Supported SQL constructs and engine details
* [SAFETY](docs/SAFETY.md) — Hazards, data safety rules, and drop policies
* [ARCHITECTURE](docs/ARCHITECTURE.md) — Internal planner and execution flow
* [CONTRIBUTING](CONTRIBUTING.md) — devenv, hooks, PR checklist
* [CHANGELOG](CHANGELOG.md)

Current tag: **v0.1.0-rc2**.

## License

Apache 2.0. See [LICENSE](LICENSE).
