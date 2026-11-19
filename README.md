# Grizzle

[![CI](https://github.com/muandane/grizzle/actions/workflows/ci.yml/badge.svg)](https://github.com/muandane/grizzle/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/muandane/grizzle.svg)](https://pkg.go.dev/github.com/muandane/grizzle)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Declarative schema migrations for Go.

Grizzle compares your live database against a plain `schema.sql` file and applies the required DDL on boot. You define the desired schema, and Grizzle calculates the minimal safe migration plan, eliminating numbered migration files (`001_init.sql`) and external migration binaries in production containers.

- **Safe by default:** Destructive drops are blocked unless explicitly allowed.
- **Production-safe DDL:** Builds PostgreSQL indexes with `CONCURRENTLY` and validates foreign keys in separate stages.
- **Cluster coordination:** Uses session-level PostgreSQL advisory locks so multiple replicas boot safely without race conditions.
- **Engine support:** PostgreSQL (14 to 18) and SQLite (3.35+, pure-Go or CGo).
- **Tooling compatibility:** Shares the same `schema.sql` file with code generators like sqlc.

---

## Quickstart

### 1. Define your desired schema

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

Grizzle supports both `SERIAL` and `GENERATED ALWAYS AS IDENTITY` columns (recommended; see [docs/SAFETY.md](docs/SAFETY.md)).

### 2. Run sync on boot

Embed `schema.sql` and call `grizzle.Sync` during application startup:

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
		AllowDrop:        false, // Fails if the diff requires dropping tables or columns
		LockTimeout:      5 * time.Second,
		StatementTimeout: 2 * time.Minute,
	})
	if err != nil {
		log.Fatalf("migration failed: %v", err)
	}
}
```

### Staged planning and approval workflows

For CI pipelines or approval gates where changes must be reviewed before execution:

```go
// 1. Generate the migration plan without applying changes
plan, err := grizzle.PlanDiff(ctx, db, opts)
if err != nil {
    log.Fatal(err)
}

// 2. Apply the plan safely; aborts if the live database changed since planning
err = grizzle.Apply(ctx, db, plan, grizzle.ApplyOpts{
    ExpectedHash: plan.Hash(),
})
```

---

## CLI

### Installation

```bash
go install github.com/muandane/grizzle/cmd/grizzle@latest
```

Pre-compiled binaries for Linux, macOS, and Windows are available on [GitHub Releases](https://github.com/muandane/grizzle/releases).

### Usage

Use the CLI for dry runs, CI drift detection, or offline linting:

```bash
# Preview changes and generate a migration plan
grizzle plan --dsn "$DATABASE_URL" --schema schema.sql --out plan.json

# Use an external dev database if the target database user cannot run CREATE SCHEMA
grizzle plan --dsn "$DATABASE_URL" --dev-url "$DEV_DATABASE_URL" --schema schema.sql

# Apply a generated plan
grizzle apply --dsn "$DATABASE_URL" --plan plan.json

# Inspect migration status and history
grizzle status --dsn "$DATABASE_URL" --schema schema.sql
grizzle history --dsn "$DATABASE_URL"

# Dump the current database schema and baseline it
grizzle dump --dsn "$DATABASE_URL" --out schema.sql
grizzle baseline --dsn "$DATABASE_URL" --schema schema.sql

# Mark an interrupted migration as resolved
grizzle repair --dsn "$DATABASE_URL" --note "manually resolved"

# Lint schema.sql offline
grizzle lint --schema schema.sql

# Run strict linting with GitHub Actions annotations in CI
grizzle lint --schema schema.sql --profile strict --format github --fail-on-warning

# Export plans to Goose or golang-migrate SQL files
grizzle export --plan plan.json --format goose --out ./migrations
grizzle export --plan plan.json --format migrate --out ./migrations
```

### Exit codes

| Code | Meaning |
| :--- | :--- |
| `0` | Success / database is in sync |
| `1` | Connection or execution error (or drift detected in `status`) |
| `2` | Blocked by an unaccepted hazard (such as an unexpected `DROP`) |
| `3` | Plan hash mismatch (database drifted after plan was generated) |
| `4` | Schema drift detected (`grizzle check`) |

### GitHub Actions Integration

Use the official Grizzle GitHub Action to lint schemas, detect drift, and preview plans with interactive pull request sticky comments:

```yaml
name: Schema CI
on: [pull_request]

permissions:
  contents: read
  pull-requests: write

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

  plan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - name: Preview migration plan and post sticky PR summary
        uses: muandane/grizzle@v0.2.0
        with:
          command: plan
          schema: schema.sql
          dsn: ${{ secrets.STAGING_DATABASE_URL }}
          comment: 'true'
          github-token: ${{ secrets.GITHUB_TOKEN }}
          extra-args: '--simulate'

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

#### Action Inputs

| Input | Description | Default |
| :--- | :--- | :--- |
| `command` | Grizzle command to execute (`lint`, `check`, `plan`, `diff`, `apply`, `export`) | `lint` |
| `schema` | Path to declarative schema SQL file | `schema.sql` |
| `dsn` | Target database connection string (`DATABASE_URL` / `POSTGRES_DSN`) | `""` |
| `dev-url` | Dev database URL for shadow-schema compilation | `""` |
| `profile` | Lint rule profile (`standard`, `strict`, `all`) | `standard` |
| `strict` | Alias for `profile: strict` | `false` |
| `fail-on-warning` | Fail lint when warnings are reported | `false` |
| `allow-drop` | Permit destructive operations | `false` |
| `comment` | Post or update sticky plan summary comment on pull requests | `true` |
| `github-token` | GitHub token for posting comments (requires `permissions: pull-requests: write`) | `${{ github.token }}` |
| `extra-args` | Extra CLI flags passed directly to grizzle (e.g. `--simulate`) | `""` |

#### Interactive PR Sticky Comment

When `comment: true` and `command: plan` (or `diff`) run on a `pull_request` event, the action automatically posts or updates a single sticky summary comment on the pull request containing:
- Metric badges for planned schema modifications (`🟢 Adds`, `🟡 Alters`, `🔴 Drops`).
- Collapsible `<details>` block with full planned SQL statements.
- Migration hazard warnings table (table rewrites, share locks, data loss risks).
- Pre-flight lock duration profiling and threshold alerts when `--simulate` is enabled.

---

## Supported engines

- **PostgreSQL (14 to 18):** Tables, partitioned tables, concurrent indexes, foreign keys, check constraints, enums, domains, composite types, routines, triggers, views, row-level security, sequences, extensions, and logical replication.
- **SQLite (3.35+):** Tables, columns, indexes, foreign keys, check constraints, and generated columns. Unsupported in-place alters run through an automatic 12-step table rebuild. Compatible with `modernc.org/sqlite` (pure Go) and `mattn/go-sqlite3` (CGo).

For the full feature matrix, safety guards, and catalog details, see [docs/FEATURES.md](docs/FEATURES.md) and [docs/SPEC.md](docs/SPEC.md).

---

## Operational notes

- **Connection pooling (PgBouncer):** Grizzle acquires session-level PostgreSQL advisory locks to coordinate migrations across application replicas. Connect directly to PostgreSQL or use **Session Pooling**. Do not run migrations through **Transaction Pooling**.
- **Destructive changes:** Dropping tables or columns requires setting `AllowDrop: true` (or `--allow-drop` in the CLI). See [docs/SAFETY.md](docs/SAFETY.md) for hazard codes and approval gates.

---

## Examples

- [postgres-stdlib](examples/postgres-stdlib): boot sync with `database/sql` and `pgx`
- [sqlite-embedded](examples/sqlite-embedded): embedded SQLite setup with pure-Go driver
- [sqlc-workflow](examples/sqlc-workflow): sharing a single `schema.sql` between sqlc and Grizzle
- [kubernetes-blueprint](examples/kubernetes-blueprint): multi-replica startup coordination
- [expand-contract](examples/expand-contract): expand, backfill, and contract pattern for zero-downtime column renames
- [roles-grants](examples/roles-grants) / [catalog-sync](examples/catalog-sync): role, grant, and publication management
- [full-catalog](examples/full-catalog): extensions, RLS, routines, triggers, and views in one schema file

---

## Documentation

- [FEATURES](docs/FEATURES.md): supported feature matrix and safety guards for PostgreSQL and SQLite
- [SPEC](docs/SPEC.md): SQL constructs, catalog introspection, and engine specification
- [SAFETY](docs/SAFETY.md): hazard classification, safety rules, and drop policies
- [LINTER](docs/LINTER_SPEC.md): schema review rules and CI linting
- [ARCHITECTURE](docs/ARCHITECTURE.md): internal planner and execution pipeline
- [CONTRIBUTING](CONTRIBUTING.md): development setup and contribution guidelines
- [CHANGELOG](CHANGELOG.md): release notes and version history

Current tag: **v0.2.0**.

---

## License

Apache 2.0. See [LICENSE](LICENSE).
