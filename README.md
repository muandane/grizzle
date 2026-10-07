# Grizzle

[![CI](https://github.com/muandane/grizzle/actions/workflows/ci.yml/badge.svg)](https://github.com/muandane/grizzle/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/muandane/grizzle.svg)](https://pkg.go.dev/github.com/muandane/grizzle)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Grizzle is a declarative, in-process database automigration library for Go. On boot it syncs PostgreSQL and SQLite schemas from a standard `schema.sql`, without numbered migration files, version collisions, or an external CLI in your production image.

```mermaid
flowchart LR
    Schema["schema.sql<br/>desired state"] --> Shadow["Shadow sandbox<br/>compile & validate"]
    Shadow -->|desired| Diff["Diff engine<br/>pure comparison"]

    Live[("Live database<br/>PostgreSQL / SQLite")] --> Lock["Advisory lock<br/>mutual exclusion"]
    Lock --> LiveInspect["Catalog inspect<br/>live state"]
    LiveInspect -->|actual| Diff

    Diff --> Plan["Migration plan<br/>steps & hash"]
    Plan --> Gate{"Hazard gate<br/>safety checks"}
    Gate -->|Approved| Apply["Apply migration<br/>tx & concurrently"]
    Apply --> Live
```

---

## Why Grizzle?

- **Standard SQL as the source of truth**: Write ordinary `CREATE TABLE` and `CREATE INDEX` statements. No Go struct tags, ORM annotations, or proprietary DSL.
- **In-process only**: Runs inside your Go binary. No Node.js, Python, Docker, or CLI sidecar required at runtime.
- **Safe defaults**: Destructive drops are blocked (`AllowDrop: false`). Risky structural changes hit a hazard gate before apply.
- **Multi-pod mutual exclusion**: Session advisory locks coordinate concurrent Kubernetes replicas, with lock-contention retry and a post-lock re-diff.
- **PostgreSQL-friendly apply**: Indexes use `CREATE INDEX CONCURRENTLY` outside transactions by default; foreign keys are added as `NOT VALID` and validated in a separate transaction.
- **SQLite parity**: 12-step table rebuild, column renames, and foreign-key checks via pure Go (`modernc.org/sqlite`).
- **Exportable plans**: Writes versioned migration artifacts for `sql`, `goose`, and `atlas` with deterministic plan hashes.

---

## Quickstart

### 1. PostgreSQL (with `pgx`)

```sql
-- schema.sql
CREATE TABLE users (
    id BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL UNIQUE,
    full_name TEXT NOT NULL,
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
	ctx := context.Background()

	db, err := sql.Open("pgx", "postgres://postgres:secret@localhost:5432/myapp?sslmode=disable")
	if err != nil {
		log.Fatalf("database connect error: %v", err)
	}
	defer db.Close()

	opts := grizzle.Options{
		SchemaSQL:        schemaSQL,
		AllowDrop:        false, // Default-deny drops
		StrictScope:      true,  // Protect unmanaged tables (asynq, postgis, etc.)
		IncludeTables:    []string{"users"},
		LockTimeout:      5 * time.Second,
		StatementTimeout: 2 * time.Minute,
	}

	// 1. Inspect and compute deterministic migration plan
	plan, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		log.Fatalf("planning failed: %v", err)
	}

	// 2. Apply plan with cryptographic approval hash verification
	err = grizzle.Apply(ctx, db, plan, grizzle.ApplyOpts{
		ExpectedHash: plan.Hash(),
	})
	if err != nil {
		log.Fatalf("migration failed: %v", err)
	}

	log.Println("Database synchronized. Starting service...")
}
```

### 2. SQLite (embedded, zero Cgo)

```sql
-- schema.sql
CREATE TABLE documents (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT NOT NULL,
    content TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_documents_created ON documents (created_at);
```

```go
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

	db, err := sql.Open("sqlite", "app.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		log.Fatalf("database open error: %v", err)
	}
	defer db.Close()

	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaSQL,
	})
	if err != nil {
		log.Fatalf("sqlite sync failed: %v", err)
	}

	log.Println("SQLite database initialized successfully.")
}
```

---

## Supported engines

| Engine | Versions | Drivers | Capabilities |
| :--- | :--- | :--- | :--- |
| **PostgreSQL** | 14, 15, 16, 17, 18 (tested in CI) | `pgx/v5`, `lib/pq`, `database/sql` | Session advisory locks, `CREATE INDEX CONCURRENTLY`, `ADD CONSTRAINT ... NOT VALID` + separate `VALIDATE CONSTRAINT`, invalid index repair, unmanaged object dependency tracking (`UNMANAGED_DEPENDENCY`). |
| **SQLite** | 3.35+ | `modernc.org/sqlite` (pure Go), `mattn/go-sqlite3` | 12-step table rebuild with trigger/view preservation, keyset batch copying, savepoint isolation, generated columns (`VIRTUAL` / `STORED`), native column renames, `PRAGMA foreign_key_check`. |

CI runs the PostgreSQL integration matrix on 14–18 (16 on pull requests).

---

## Contributing

Full guidelines: [CONTRIBUTING.md](CONTRIBUTING.md).

Local development uses [devenv](https://devenv.sh) (Nix) as the single source of truth for Go, golangci-lint, PostgreSQL 16, git hooks, and quality-gate scripts. There is no Makefile; do not add one.

### Prerequisites

1. Install [devenv](https://devenv.sh/getting-started/) (`2.x`) and [direnv](https://direnv.net/).
2. From the repo root, run `direnv allow` (or `devenv shell`). That provisions tools and installs local git hooks.
3. Run `devenv up` to start the background PostgreSQL service (required for integration tests).

`DATABASE_URL` / `POSTGRES_DSN` default to:

```text
postgres://127.0.0.1:5432/grizzle_test?sslmode=disable
```

### Day-to-day commands

These are available inside the devenv shell and mirror CI:

| Command | What it does |
| :--- | :--- |
| `fmt` | Format both Go modules |
| `vet` | `go vet` on both modules |
| `lint` | golangci-lint on both modules |
| `test` | Unit tests with the race detector |
| `test-integration` | Integration tests against local PostgreSQL (needs `devenv up`) |
| `ci` | Full local gate: lint + vet + unit + integration |
| `golden-update` | Regenerate golden plan/export fixtures after intentional changes |
| `db-shell` / `db-reset` / `clean` | psql into the test DB, wipe schema, remove test artifacts |

### Before you open a PR

1. Respect the layering rules in [CONTRIBUTING.md](CONTRIBUTING.md) (pure core must stay free of I/O).
2. Pass `lint` and `ci` (including `-race`).
3. Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, …).
4. Document public API changes in `docs/SPEC.md` and `CHANGELOG.md`.
5. Do not log or commit secrets or connection credentials.

Git hooks (installed on shell entry):

| Hook | Checks |
| :--- | :--- |
| `pre-commit` | `gofmt`, `go vet`, `golangci-lint` (both modules) |
| `pre-push` | `go test -race -count=1 ./...` (both modules) |

Integration tests are not hooked (they need `devenv up`). Run `ci` before a PR that touches `internal/exec`, `internal/history`, or the public API.

---

## Five safety defaults

| Capability | TypeORM (`synchronize`) | Drizzle (`drizzle-kit push`) | Atlas CLI | Ent (`entgo.io`) | Grizzle |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Runtime topology** | In-process Node.js | External Node CLI | External Go binary / Docker | In-process Go ORM | **In-process Go library** |
| **Distributed locking** | None (races on multi-pod boot) | None (developer CLI) | Advisory locks | Custom driver hook | **`pg_advisory_lock` + post-lock re-diff** |
| **Default drop policy** | Drops unmapped tables/cols | Prompts in TTY; drops with `--force` | Configurable flags | Drops disabled (`WithDropColumn(false)`) | **Hard-blocked (`AllowDrop: false`)** |
| **Shadow validation** | None (runs on live DB) | None (runs on live DB) | Dev DB container | In-memory parser | **Isolated shadow schema (`_grizzle_shadow`)** |
| **Pre-execution hazard gate** | None | None | Analyzer rules (Pro/Cloud) | None | **Structural `Plan.Hazards()` gate** |
| **Plan hash & drift check** | None | None | Directory checksums | None | **`Plan.Hash()` + `Check()`** |

Details and citations: [docs/SAFETY.md](docs/SAFETY.md).

---

## Schema linting

`grizzle.Lint` statically checks the desired schema IR (pure, database-free, deterministic) before anything reaches a migration. Diagnostics carry a rule ID, severity (`ERROR` / `WARNING` / `INFO`), and target; render them as text, JSON, or GitHub Actions annotations (`LintFormatText` / `LintFormatJSON` / `LintFormatGitHub`).

| Rule | Severity | Check |
| :--- | :--- | :--- |
| `L001` | `ERROR` | Table has no primary key (breaks logical replication; partitions skipped) |
| `L002` | `WARNING` | Foreign key columns not covered by any index (parent changes sequential-scan the table) |
| `L003` | `WARNING` | Table / column / index / enum names not lowercase snake_case |
| `L004` | `WARNING` | Legacy `SERIAL` / `nextval` default instead of `GENERATED ALWAYS AS IDENTITY` |
| `L005` | `WARNING` | CHECK constraint name not lowercase snake_case |
| `L006` | `WARNING` | Duplicate CHECK constraint expression on the same table |
| `L007` | `INFO` | CHECK constraint relies on PostgreSQL's auto-generated name; prefer an explicit `CONSTRAINT name CHECK` |

```go
// LintSchema with no rules uses DefaultLintRules() (L001..L007).
diags := grizzle.LintSchema(schemaIR)
if grizzle.LintHasErrors(diags) {
    log.Fatalf("schema lint failed:\n%s", mustFormatLint(diags))
}
```

Details: [docs/SPEC.md](docs/SPEC.md#6-schema-linting).

---

## Architecture

Dependency direction is one-way:

```mermaid
graph TD
    classDef pure fill:#e8f5e9,stroke:#2e7d32,stroke-width:1px;

    API["grizzle (public API)<br/>Sync · PlanDiff · Apply · Check · Export"]

    API -->|runs| Exec["exec<br/>locks, tx, retries"]
    API -->|formats| Export["export<br/>SQL / Goose / Atlas"]:::pure

    Exec -->|audits| History["history<br/>audit table"]
    Exec -->|delegates| Dialect["dialect (postgres, sqlite)<br/>introspect + render DDL"]
    Exec -->|computes| Diff["diff<br/>desired vs actual"]:::pure

    History -->|queries| Dialect
    Dialect -->|renders| Plan["plan<br/>steps, hazards, hash"]:::pure
    Dialect -->|builds| Schema["schema + scope<br/>models, filters"]:::pure

    Diff -->|generates| Plan
    Diff -->|compares| Schema
    Export -->|serializes| Plan
```

### Runtime flow

```mermaid
flowchart LR
    Introspect["1. Introspect<br/>catalog state"] -->|SchemaIR| Diff["2. Diff<br/>compute delta"]
    Diff -->|Changes| Plan["3. Plan<br/>hazard gating"]
    Plan -->|Steps| Render["4. Render<br/>dialect DDL"]
    Render -->|SQL| Exec["5. Exec<br/>locks & tx groups"]
```

- **Pure core** (green: `diff`, `plan`, `schema`, `scope`, `export`): No `database/sql`, context, network, or file I/O. Deterministic and unit-testable.
- **Dialect layer** (`dialect/postgres`, `dialect/sqlite`): Catalog introspection and dialect-specific DDL.
- **Execution layer** (`exec`, `history`): Connection pools, advisory locks, timeouts, transaction grouping, failure recording.
- **Public facade** (`grizzle`): Minimal wiring and public type aliases.

---

## When to use Grizzle vs goose vs Atlas

| Scenario | Recommend | Why |
| :--- | :--- | :--- |
| **Go app boot automigration** | **Grizzle** | In-process on boot, single binary, standard `schema.sql`, advisory locking, no deployment sidecar. |
| **Hand-written sequential SQL** | **goose** | Manual data transforms, ETL backfills, or numbered scripts (`001_init.sql`). Bootstrap with `grizzle export --format goose`. |
| **Polyglot / Terraform / CI pipelines** | **Atlas** | Multi-language stacks, Atlas Kubernetes Operator, or Atlas Cloud governance. Bootstrap with `grizzle export --format atlas`. |

---

## Stability (v0.1.0)

- **Stable enough for production use**: `Sync`, `PlanDiff`, `Apply`, `Check`, `Export`, both dialects, advisory locking, invalid index recovery, unmanaged object protection, and hazard gates are covered by unit and integration tests.
- **Experimental**: Column renames (`Options.Renames`) and expand-and-contract migrations (`Options.ExpandContract`, `Options.Backfill`) work and are golden-tested, but are marked `// Experimental:` because their config shapes may change before v1.0.

---

## CLI

```bash
# Generate deterministic migration plan JSON
grizzle plan --dsn "$DATABASE_URL" --schema schema.sql --out plan.json

# Apply pre-approved plan with strict drift verification
grizzle apply --dsn "$DATABASE_URL" --plan plan.json --accept-hazard DROP_COLUMN

# Check live database for schema drift in CI (exits 0 if clean, 4 on drift)
grizzle check --dsn "$DATABASE_URL" --schema schema.sql

# Export planned migration to Goose or Atlas format
grizzle export --plan plan.json --format goose --out ./migrations

# Experimental: zero-downtime expand/contract planning with explicit renames
# (expand: new columns added alongside existing; contract: separate drop plan)
grizzle plan --dsn "$DATABASE_URL" --schema schema.sql --out expand.json \
  --rename users.full_name=display_name --expand-contract
grizzle plan --dsn "$DATABASE_URL" --schema schema.sql --out contract.json \
  --rename users.full_name=display_name --allow-drop
```

`--rename old=new` (optionally table-qualified `table.old=new`) can be repeated and is accepted by `plan`, `apply`, and `check`. `--expand-contract` stages new columns alongside existing ones and defers drops to a separate contract plan. Backfilling renamed columns is library-only (`Options.Backfill`); see [examples/expand-contract](examples/expand-contract).

### Exit codes

| Code | Meaning |
| :--- | :--- |
| `0` | Success / clean schema / no-op |
| `1` | Execution or connection error |
| `2` | Blocked by unaccepted hazard |
| `3` | Plan hash drift during `apply` (`ErrPlanDrift`) |
| `4` | Schema drift during `check` |

---

## Examples

- [Standard Library PostgreSQL](examples/postgres-stdlib): `database/sql` + `pgx` startup automigration.
- [Embedded SQLite](examples/sqlite-embedded): In-process SQLite, zero Cgo.
- [sqlc Workflow](examples/sqlc-workflow): Shared `schema.sql` with sqlc query generation.
- [Native pgxpool](examples/postgres-pgxpool): `*pgxpool.Pool` for queries via `stdlib.OpenDBFromPool`.
- [Kubernetes Blueprint](examples/kubernetes-blueprint): Multi-replica rolling update coordination.
- [Expand/Contract](examples/expand-contract): Zero-downtime column rename with staged expand, library-only backfill, and a separate contract plan (experimental).

---

## Documentation

- [Safety invariants & competitor analysis](docs/SAFETY.md)
- [System architecture](docs/ARCHITECTURE.md)
- [Engine specification & API reference](docs/SPEC.md)
- [Planned engine features](docs/ENGINE_FEATURES.md)
- [Changelog](CHANGELOG.md)
- [Contributing](CONTRIBUTING.md)

---

## License

Apache License 2.0. See [LICENSE](LICENSE).
