# Grizzle

Grizzle provides declarative, in-process database schema automigration for Go applications. It synchronizes PostgreSQL and SQLite schemas directly from standard `schema.sql` files at application boot, eliminating migration sequence files, version collisions, and external CLI tools.

## Problems Grizzle solves

1. **Migration sequence conflicts.** When multiple developers generate numbered migration files on different Git branches (e.g. `0005_add_users.sql` and `0005_add_teams.sql`), merging to the main branch causes file name collisions and execution ordering ambiguities. Grizzle diffs the desired state against the live catalog directly.
2. **Schema duplication in ORMs.** Libraries such as GORM and Ent require declaring schemas using Go struct tags or Go DSLs. GORM auto-migration only adds missing columns and skips column type alterations or drop detection. Grizzle uses standard SQL DDL as the single source of truth.
3. **External binary requirements.** Tools such as Atlas, Prisma, and Drizzle require standalone CLI binaries, Node.js runtimes, or pre-migration Docker containers. Grizzle runs entirely within the compiled Go application binary.

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

Embed the file and call `grizzle.Sync` in `main.go`:

```go
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"

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

	// Run declarative migration on startup
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaSQL,
		AllowDrop: false, // Prevents accidental data destruction in production
		ExcludeTables: []string{"asynq_*", "temporal_*"}, // Protects worker queue tables
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
* **Automatic dialect detection**: Identifies the database engine from the driver type without manual configuration.
* **Multi-pod safety**: Acquires PostgreSQL transactional advisory locks (`pg_advisory_xact_lock`) to prevent race conditions during concurrent replica startups.
* **SQLite 12-step rebuild**: Executes table recreation procedures to modify column types and drop constraints without data loss.
* **Non-destructive defaults**: `AllowDrop: false` halts execution if columns or tables are missing from `schema.sql`. Granular flags (`AllowDropTable`, `AllowDropColumn`, `AllowDropIndex`, `AllowDropFK`) allow selective overrides.
* **Third-party table preservation**: Protects external tables (PostGIS metadata, task queues like `asynq` or `pgboss`) via `ExcludeTables` patterns and `IncludeTables` whitelisting.
* **Static hazard analysis**: `Plan.Hazards()` flags data-loss risks, missing defaults on `NOT NULL` columns, and table locking operations before applying changes.
* **Terminal visualization**: Renders colored migration diffs and hazards using `Plan.Format(os.Stdout, true)`.

## Documentation index

* [Safety analysis & production comparison](docs/SAFETY.md): Comparison of Grizzle, Drizzle ORM, Atlas, Ent, and TypeORM, detailing production invariants and guarantees.
* [System architecture](docs/ARCHITECTURE.md): Component diagrams, state transitions, and execution flow.
* [Specification](docs/SPEC.md): API contract, configuration options, and error taxonomy.
* [Design notes](docs/DESIGN.md): PostgreSQL catalog inspection, SQLite rebuild engine, and type normalization algorithms.
* [Roadmap](docs/ROADMAP.md): Development milestones and completed implementation phases.

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
