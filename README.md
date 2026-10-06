# Grizzle

[![CI](https://github.com/muandane/grizzle/actions/workflows/ci.yml/badge.svg)](https://github.com/muandane/grizzle/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/muandane/grizzle.svg)](https://pkg.go.dev/github.com/muandane/grizzle)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Grizzle is a declarative, in-process database automigration engine for Go applications. It synchronizes PostgreSQL and SQLite database schemas directly from standard `schema.sql` definitions on boot, eliminating manual migration sequence files, version collisions, and external runtime CLI dependencies.

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

- **Standard SQL as single source of truth**: No Go struct tags, ORM annotations, or proprietary DSLs. Write standard `CREATE TABLE` and `CREATE INDEX` statements.
- **Zero external dependencies**: Runs entirely in-process within your compiled Go binary. Zero Node.js, Python, Docker, or external CLI requirements in production container images.
- **Production safety by default**: Destructive drops are hard-blocked (`AllowDrop: false`). Dangerous structural alterations trigger blocking hazard gates.
- **Distributed multi-pod mutual exclusion**: Dedicated session-level advisory locks prevent migration races across concurrent Kubernetes replicas, with automatic lock contention retry and post-lock re-diffing.
- **Safe PostgreSQL execution**: Index creation defaults to `CREATE INDEX CONCURRENTLY` outside transactions; foreign keys are staged as `NOT VALID` and validated in a separate transaction to avoid extended table locks.
- **Complete SQLite parity**: Supports table rebuild procedures (12-step SQLite table rebuild), column renames, and foreign key verification via pure Go (`modernc.org/sqlite`).
- **Exportable artifacts**: Generates versioned migration files for `sql`, `goose`, and `atlas` formats with deterministic plan hashes.

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

### 2. SQLite (Embedded, Zero Cgo)

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

	// Apply schema automatically in a single call
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

## Five Safety Defaults Comparison

| Capability | TypeORM (`synchronize`) | Drizzle (`drizzle-kit push`) | Atlas CLI | Ent (`entgo.io`) | Grizzle |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Runtime Topology** | In-process Node.js | External Node CLI | External Go binary / Docker | In-process Go ORM | **In-process Go library** |
| **Distributed Locking** | None (races on multi-pod boot) | None (developer CLI) | Advisory locks | Custom driver hook | **`pg_advisory_lock` + post-lock re-diffing** |
| **Default Drop Policy** | Drops unmapped tables/cols | Prompts in TTY; drops with `--force` | Configurable flags | Drops disabled (`WithDropColumn(false)`) | **Hard-blocked (`AllowDrop: false`)** |
| **Shadow Validation** | None (runs on live DB) | None (runs on live DB) | Dev DB container | In-memory parser | **Isolated shadow schema (`_grizzle_shadow`)** |
| **Pre-execution Hazard Gate** | None | None | Analyzer rules (Pro/Cloud) | None | **Structural `Plan.Hazards()` blocking gate** |
| **Plan Hash & Drift Check** | None | None | Directory checksums | None | **Cryptographic `Plan.Hash()` + `Check()`** |

*For a detailed architectural breakdown and benchmark citations, see [docs/SAFETY.md](docs/SAFETY.md).*

---

## Supported Engines

| Engine | Version | Dialect Driver | Key Capabilities |
| :--- | :--- | :--- | :--- |
| **PostgreSQL** | 13, 14, 15, 16, 17 | `pgx/v5`, `lib/pq`, `database/sql` | Session advisory locks, `CREATE INDEX CONCURRENTLY`, `ADD CONSTRAINT ... NOT VALID` with separate `VALIDATE CONSTRAINT`, automated invalid index repair, unmanaged object dependency tracking (`UNMANAGED_DEPENDENCY`). |
| **SQLite** | 3.35+ | `modernc.org/sqlite` (pure Go), `mattn/go-sqlite3` | 12-step table rebuild engine, generated columns (`VIRTUAL` / `STORED`), native column renames, `PRAGMA foreign_key_check` validation. |

---

## Architecture

Grizzle enforces a strict one-way dependency architecture:

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

- **Pure Core** (shaded green: `diff`, `plan`, `schema`, `scope`, `export`): Free of `database/sql`, context, network, or file I/O. Deterministic and unit-testable.
- **Dialect Layer** (`dialect/postgres`, `dialect/sqlite`): Encapsulates catalog introspection and dialect-specific DDL syntax.
- **Execution Layer** (`exec`, `history`): Coordinates connection pools, advisory locks, timeouts, transaction grouping, and failure recording.
- **Public Facade** (`grizzle`): Minimal wiring surface and public type aliases.

---

## When to use Grizzle vs goose vs Atlas

| Scenario | Recommend | Rationale |
| :--- | :--- | :--- |
| **Go application boot automigration** | **Grizzle** | Runs in-process on boot, single binary, standard `schema.sql`, distributed advisory locking, zero deployment sidecars. |
| **Imperative hand-crafted SQL migrations** | **goose** | When migrations require manual data transformations (`UPDATE users SET legacy = false`), complex ETL backfills, or strict sequential numbered scripts (`001_init.sql`). *Tip: Use `grizzle export --format goose` to bootstrap initial goose files.* |
| **Polyglot teams & Terraform / CI/CD pipelines** | **Atlas** | When managing multiple language stacks (Node, Python, Java), deploying via Atlas Kubernetes Operator, or using Atlas Cloud team governance. *Tip: Use `grizzle export --format atlas` to generate compatible schemas.* |

---

## Stability Note (v0.1.0)

- **Production-Ready Core**: The declarative automigration pipeline (`Sync`, `PlanDiff`, `Apply`, `Check`, `Export`), PostgreSQL and SQLite dialects, advisory locking, invalid index recovery, unmanaged object protection, and hazard gates are production-tested and covered by rigorous integration suites.
- **Experimental APIs**: Column renames (`Options.Renames`) and staged zero-downtime expand-and-contract migrations (`Options.ExpandContract`, `Options.Backfill`) are fully functional and golden-tested, but marked `// Experimental:` as their configuration signatures may evolve prior to v1.0.

---

## CLI Usage

Grizzle includes a lightweight CLI for CI/CD automation and artifact export:

```bash
# Generate deterministic migration plan JSON
grizzle plan --dsn "$DATABASE_URL" --schema schema.sql --out plan.json

# Apply pre-approved plan with strict drift verification
grizzle apply --dsn "$DATABASE_URL" --plan plan.json --accept-hazard DROP_COLUMN

# Check live database for schema drift in CI (exits 0 if clean, 4 on drift)
grizzle check --dsn "$DATABASE_URL" --schema schema.sql

# Export planned migration to Goose or Atlas format
grizzle export --plan plan.json --format goose --out ./migrations
```

### Exit Codes

- `0`: Success / clean schema / no-op
- `1`: Execution or connection error
- `2`: Blocked by unaccepted hazard
- `3`: Plan hash drift during `apply` (`ErrPlanDrift`)
- `4`: Schema drift detected during `check`

## Examples and deployment patterns

- [Standard Library PostgreSQL](examples/postgres-stdlib): Minimal startup automigration with `database/sql` and `pgx`.
- [Embedded SQLite](examples/sqlite-embedded): In-process embedded SQLite automigration with zero Cgo.
- [sqlc Workflow](examples/sqlc-workflow): Single source of truth workflow pairing `schema.sql` with `sqlc` query generation.
- [Native pgxpool](examples/postgres-pgxpool): Using `*pgxpool.Pool` for application queries with `stdlib.OpenDBFromPool`.
- [Kubernetes Blueprint](examples/kubernetes-blueprint): Production multi-replica deployment manifest with rolling update coordination.

---

## Documentation

- [Safety Invariants & Competitor Analysis](docs/SAFETY.md)
- [System Architecture](docs/ARCHITECTURE.md)
- [Engine Specification & API Reference](docs/SPEC.md)
- [Planned Engine Features](docs/ENGINE_FEATURES.md)
- [Changelog](CHANGELOG.md)



---

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
