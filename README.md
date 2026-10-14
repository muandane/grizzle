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
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      VARCHAR(255) NOT NULL UNIQUE,
    full_name  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_users_created_at ON users (created_at);
```

> **Note:** Grizzle supports both `SERIAL` and standard-compliant identity columns, but recommends `GENERATED ALWAYS AS IDENTITY` for new tables (see [SAFETY.md](docs/SAFETY.md)).


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

### Installation

```bash
go install github.com/muandane/grizzle/cmd/grizzle@latest
```

Pre-compiled binaries for Linux, macOS, and Windows are also available on [GitHub Releases](https://github.com/muandane/grizzle/releases).

### Usage

Use the CLI for CI drift checks, dry runs, or exporting plans to standard migration tools:

```bash
# Preview changes and generate a migration plan
grizzle plan --dsn "$DATABASE_URL" --schema schema.sql --out plan.json

# Use an external dev database URL if target DB lacks CREATE SCHEMA
grizzle plan --dsn "$DATABASE_URL" --dev-url "$DEV_DATABASE_URL" --schema schema.sql

# Apply a generated plan safely
grizzle apply --dsn "$DATABASE_URL" --plan plan.json

# Inspect migration status and history
grizzle status --dsn "$DATABASE_URL" --schema schema.sql
grizzle history --dsn "$DATABASE_URL"

# Reverse engineer an existing database and baseline it
grizzle dump --dsn "$DATABASE_URL" --out schema.sql
grizzle baseline --dsn "$DATABASE_URL" --schema schema.sql

# Resolve an interrupted or failed migration
grizzle repair --dsn "$DATABASE_URL" --note "manually resolved"

# Statically lint schema.sql offline (pure in-memory shadow, no DB connection needed)
grizzle lint --schema schema.sql

# Run strict linting with GitHub Actions annotations in CI
grizzle lint --schema schema.sql --profile strict --format github --fail-on-warning

# Convert a plan to Goose or golang-migrate migrations if you need an exit hatch
grizzle export --plan plan.json --format goose --out ./migrations
grizzle export --plan plan.json --format migrate --out ./migrations
```

### Exit Codes

| Code | Meaning |
| --- | --- |
| `0` | Success / database is in sync |
| `1` | Connection or execution error (or drift detected in `status`) |
| `2` | Blocked by an unaccepted hazard (e.g., unexpected DROP) |
| `3` | Plan hash mismatch (live database changed after plan was generated) |
| `4` | Schema drift detected (`grizzle check`) |

### GitHub Actions Integration

Use the official Grizzle GitHub Action to lint schemas, detect drift, and preview plans in CI workflows:

```yaml
name: Schema CI
on: [pull_request]

jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - name: Lint declarative schema
        uses: muandane/grizzle@v0.2.0
        with:
          command: lint
          schema: schema.sql
          strict: true
          format: github

  drift-check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - name: Check for schema drift
        uses: muandane/grizzle@v0.2.0
        with:
          command: check
          schema: schema.sql
          dsn: ${{ secrets.STAGING_DATABASE_URL }}
```

---

## Supported Features

### PostgreSQL (14–18)

| Category | Supported Capabilities | Safety & Scope Guards |
| :--- | :--- | :--- |
| **Tables & Columns** | Table creation/drop, column add/drop/rename, type alters (`USING`), nullability, defaults, generated columns, partitioned tables (declarative attach/detach). | Identity and serial sequences preserved; unmanaged tables outside target schema untouched. |
| **Indexes & Constraints** | `CREATE INDEX CONCURRENTLY`, partial/unique/expression/BRIN/GIN/GiST indexes; foreign keys (`NOT VALID` + `VALIDATE`), `CHECK` constraints (staged validation, normalized expressions). | Backing indexes for primary keys and unique constraints managed via constraint definition. |
| **Data Types** | Enums (`ADD VALUE`, `RENAME VALUE`), domains (`CHECK` constraints), custom range types, composite types (`ADD`/`DROP ATTRIBUTE`). | System and extension types preserved; dropped enum values flagged as critical hazards. |
| **Sequences** | Standalone sequences, parameters (`INCREMENT`, `MIN`/`MAXVALUE`, `CACHE`, `CYCLE`), ownership (`OWNED BY table.col`), `DROP SEQUENCE IF EXISTS`. | Implicit identity/serial sequences and unmanaged external sequences are preserved. |
| **Routines & Triggers** | Functions and procedures (`CREATE OR REPLACE`, signature drift drop/recreate), per-table triggers, user-defined aggregates. | Built-in and extension functions untouched; window functions and hypothetical aggregates preserved. |
| **Views** | Standard views (`CREATE OR REPLACE` for append-only, drop/recreate for structural alters), materialized views (drop/recreate + refresh). | System catalogs and unmanaged views untouched. |
| **Security & Access** | Row-Level Security (`ENABLE`/`FORCE`), RLS policies (`CREATE`/`ALTER`/`DROP POLICY`), roles and grants (via `RolesSQL` / `SchemaSQL`). | Unmanaged roles and `PUBLIC` grants preserved; marker comment `grizzle-managed` required to drop roles. |
| **Cluster & Replication** | Extensions (`CREATE EXTENSION`), logical publications, subscriptions, replication slots, DDL event triggers. | Physical slots and unmanaged publications/triggers without `grizzle-managed` marker are untouched. |

> [!NOTE]
> Plan hashes (`Plan.Hash()`) are deterministic across versions. Detailed catalog queries, hazard taxonomy, and engine internals are documented in [docs/SPEC.md](docs/SPEC.md).

### SQLite (3.35+)

* Tables, columns, foreign keys, `CHECK` constraints, indexes, and generated columns.
* Automatic 12-step table rebuilds for schema changes SQLite cannot execute in-place via `ALTER TABLE`.
* Supports both `modernc.org/sqlite` (pure Go) and `mattn/go-sqlite3` (CGo).
* SQLite does not support `COMMENT ON` or Row-Level Security (PostgreSQL-only).

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
* [LINTER](docs/LINTER_SPEC.md) — SQL review rules, naming conventions, and linting catalog
* [ARCHITECTURE](docs/ARCHITECTURE.md) — Internal planner and execution flow
* [CONTRIBUTING](CONTRIBUTING.md) — devenv, hooks, PR checklist
* [CHANGELOG](CHANGELOG.md)

Current tag: **v0.2.0**.

## License

Apache 2.0. See [LICENSE](LICENSE).
