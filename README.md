# Grizzle

Grizzle provides declarative, in-process database schema automigration for Go applications. It synchronizes PostgreSQL and SQLite schemas directly from standard `schema.sql` files at application boot, eliminating migration sequence files, version collisions, and external CLI tools.

## Problems Grizzle solves

1. **Migration sequence conflicts.** When multiple developers generate numbered migration files on different Git branches (e.g. `0005_add_users.sql` and `0005_add_teams.sql`), merging to the main branch causes file name collisions and execution ordering ambiguities. Grizzle diffs the desired state against the live catalog directly.
2. **Schema duplication in ORMs.** Libraries such as GORM and Ent require declaring schemas using Go struct tags or Go DSLs. GORM auto-migration only adds missing columns and skips column type alterations or drop detection. Grizzle uses standard SQL DDL as the single source of truth.
3. **External binary requirements.** Tools such as Atlas, Prisma, and Drizzle require standalone CLI binaries, Node.js runtimes, or pre-migration Docker containers. Grizzle runs entirely within the compiled Go application binary.
4. **Multi-replica race conditions.** When multiple application instances boot concurrently, uncoordinated DDL execution leads to deadlocks and failed deployments. Grizzle coordinates instances with distributed advisory locking, lock timeouts, and exponential backoff retry.

## Quick start

Define your database schema in standard SQL:

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

Embed the file and call `grizzle.Apply` (or `grizzle.Sync`) in `main.go`:

```go
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/yourorg/grizzle"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	ctx := context.Background()

	db, err := sql.Open("pgx", "postgres://postgres:password@localhost:5432/myapp?sslmode=disable")
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer db.Close()

	opts := grizzle.Options{
		SchemaSQL:        schemaSQL,
		AllowDrop:        false, // Prevents accidental data destruction in production
		StrictScope:      true,  // Requires explicit IncludeTables whitelist
		IncludeTables:    []string{"users"},
		ExcludeTables:    []string{"asynq_*", "temporal_*"}, // Protects worker queue tables
		LockTimeout:      5 * time.Second,
		StatementTimeout: 2 * time.Minute,
	}

	// 1. Inspect and compute deterministic migration plan
	plan, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		log.Fatalf("plan failed: %v", err)
	}

	// 2. Apply plan with cryptographic approval hash verification
	err = grizzle.Apply(ctx, db, plan, grizzle.ApplyOpts{
		ExpectedHash: plan.Hash(),
	})
	if err != nil {
		log.Fatalf("migration failed: %v", err)
	}

	log.Println("Database synchronized. Starting server...")
}
```

## Features

* **Zero external dependencies**: Runs completely in-process within your compiled Go binary. Zero Cgo.
* **PostgreSQL and SQLite engines**: Supports PostgreSQL 13+ and SQLite 3.35+ (via `modernc.org/sqlite`).
* **Deterministic plan hashing & drift detection**: `Plan.Hash()` computes a SHA-256 digest of migration steps; `Apply` verifies post-lock state against `ExpectedHash`, and `Check(ctx, db, opts)` inspects drift read-only.
* **Blocking hazard gating**: Critical hazards (`DROP_TABLE`, `DROP_COLUMN`, `TYPE_NARROW`, `RENAME_AMBIGUOUS`) fail execution unless accepted via `Options.AcceptHazards`. Evaluated structurally without SQL string matching.
* **Distributed locking with retry**: Acquires advisory locks (`pg_advisory_xact_lock` or session-level advisory locks), recomputes plans post-lock, and automatically retries with exponential backoff and jitter on lock contention (`55P03`).
* **Non-blocking concurrent indexes**: PostgreSQL builds new indexes using `CREATE INDEX CONCURRENTLY` by default outside transactions, minimizing table locks.
* **Safe foreign key validation**: Foreign keys are added as `NOT VALID` and verified in a subsequent `VALIDATE CONSTRAINT` step to eliminate prolonged share-row-exclusive locks.
* **Staged expand-and-contract column renames**: Detects ambiguous renames, supports explicit mappings via `Options.Renames`, and stages dual-column additions with `Options.ExpandContract: true` for zero-downtime migrations.
* **Non-destructive defaults**: `AllowDrop: false` halts execution if columns or tables are missing from `schema.sql`. Granular flags (`AllowDropTable`, `AllowDropColumn`, `AllowDropIndex`, `AllowDropFK`) allow selective overrides.
* **Audit history tracking**: Automatically records applied migration plans, hashes, timestamps, durations, and steps into `grizzle_history`.
* **SQLite 12-step rebuild**: Executes table recreation procedures to modify column types and drop constraints safely.
* **Terminal visualization**: Renders colored migration diffs and hazards using `Plan.Format(os.Stdout, true)`.

## Documentation index

* [Safety analysis & production comparison](docs/SAFETY.md): Detailed comparison with Drizzle ORM, Atlas, Ent, and TypeORM, documenting safety invariants, competitor references, and production guarantees.
* [System architecture](docs/ARCHITECTURE.md): Layered dependency architecture, component interactions, and execution sequence diagrams.
* [Specification](docs/SPEC.md): Complete API contract, configuration options, and error taxonomy.
* [Design notes](docs/DESIGN.md): PostgreSQL catalog inspection, SQLite rebuild engine, and type normalization algorithms.

## Development environment

Grizzle uses [devenv](https://devenv.sh/) to provide a reproducible development shell with Go 1.27, PostgreSQL 16, and test utilities.

```bash
# 1. Enter the dev shell
devenv shell

# 2. Start PostgreSQL daemon
devenv up

# 3. Development commands
test-all    # Run tests with race detection (-race)
lint        # Run golangci-lint
db-shell    # Open psql on grizzle_test
db-reset    # Reset public schema
clean       # Remove generated SQLite databases and test artifacts
```
