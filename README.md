# Grizzle 🐻⚡

> **Declarative, in-process database schema automigration for Go.**  
> Define your schema once in standard `schema.sql`. No migration sequence files. No merge conflicts. No external CLI or Docker containers required.

---

## The Problem Grizzle Solves

1. **Migration Sequence Conflicts (Goose, Flyway, golang-migrate):**  
   When multiple developers create migrations concurrently on different Git branches (e.g. `0005_add_users.sql` and `0005_add_teams.sql`), merging to `main` results in version number collisions or non-deterministic execution order.
2. **Heavyweight ORMs (GORM, Ent):**  
   Forces you to define schemas using Go struct tags instead of SQL. GORM’s `AutoMigrate` only adds missing columns and cannot reliably handle type modifications, drop detection, or complex index diffs.
3. **Complex CLI Workflows (Atlas, Prisma, Drizzle-Kit):**  
   Require external binaries, Node.js runtimes, Docker containers, or pre-deploy CLI steps that complicate container images and developer workflows.

---

## The Grizzle Approach

With Grizzle, your single source of truth is plain SQL (e.g., `schema.sql`):

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

You embed it and call Grizzle directly in `main.go` on startup:

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
		log.Fatalf("failed to connect to db: %v", err)
	}
	defer db.Close()

	// In-process automigration on application boot
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaSQL,
		AllowDrop: false, // Prevents accidental data loss in production
	})
	if err != nil {
		log.Fatalf("grizzle sync failed: %v", err)
	}

	log.Println("Database schema is in sync! Starting app...")
	// Start your HTTP / gRPC server...
}
```

---

## Key Features

* 🚀 **Zero External Binaries**: Runs completely in-process within your compiled Go binary.
* 🐘 **PostgreSQL & 🪶 SQLite**: First-class support for PostgreSQL 13+ and SQLite 3.35+ (via pure-Go `modernc.org/sqlite`, zero Cgo required).
* 🔍 **Automatic Dialect Detection**: Automatically detects PostgreSQL or SQLite from your `*sql.DB` connection driver without configuration.
* 🛡️ **Multi-Replica Safe**: Leverages transactional PostgreSQL advisory locks (`pg_advisory_xact_lock`) to eliminate race conditions when multiple pods boot simultaneously.
* 🔄 **SQLite 12-Step Rebuild Engine**: Seamlessly handles SQLite column modifications and drops while preserving 100% of existing row data and foreign keys.
* 🔒 **Fine-Grained Drop Protection**: `AllowDrop: false` halts boot on destructive operations. Granular switches (`AllowDropTable`, `AllowDropColumn`, `AllowDropIndex`, `AllowDropFK`) enable surgical permission controls.
* 📊 **Terminal Visualizer**: Inspect diffs before executing with `PlanDiff()`, colorized terminal output (`Plan.Format(os.Stdout, true)`), and structured logging via standard `log/slog`.
* 🧩 **Git Merge Friendly**: Changes to `schema.sql` merge naturally like any code file—no sequence number coordination.

---

## Documentation Index

* 📐 **[System Architecture](docs/ARCHITECTURE.md)**: Architectural diagrams, state machine, and component flow.
* 📋 **[Functional & Technical Spec](docs/SPEC.md)**: Guarantees, API contracts, safety models, and limits.
* 🛠️ **[Deep Technical Design](docs/DESIGN.md)**: PostgreSQL catalog queries, diffing algorithms, and type normalization.
* 🗺️ **[Development Roadmap](docs/ROADMAP.md)**: Step-by-step phases to build, test, and ship Grizzle.

---

## Development Environment (Nix + devenv)

Grizzle includes a reproducible Nix developer environment powered by [devenv](https://devenv.sh/). It automatically provisions Go, PostgreSQL 16, `golangci-lint`, and client tools.

```bash
# 1. Enter the dev shell (or let direnv load it automatically)
devenv shell

# 2. Start local PostgreSQL daemon in background
devenv up

# 3. Available helper commands inside the shell
test-all    # Run all unit and integration tests with -race
lint        # Run golangci-lint
db-shell    # Open psql connected to the local test database
db-reset    # Cleanly reset the public schema in grizzle_test
```

