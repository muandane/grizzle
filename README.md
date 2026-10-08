# Grizzle

[![CI](https://github.com/muandane/grizzle/actions/workflows/ci.yml/badge.svg)](https://github.com/muandane/grizzle/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/muandane/grizzle.svg)](https://pkg.go.dev/github.com/muandane/grizzle)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Declarative schema sync for Go. Point it at a `schema.sql` and Grizzle brings PostgreSQL or SQLite in line on boot. No numbered migration files, no external CLI in the production image.

Drops are off by default. Risky changes need an explicit hazard accept. Concurrent pods share a session advisory lock and re-diff after acquiring it.

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
	db, err := sql.Open("pgx", "postgres://postgres:secret@localhost:5432/myapp?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		SchemaSQL:        schemaSQL,
		AllowDrop:        false,
		StrictScope:      true,
		IncludeTables:    []string{"users"},
		LockTimeout:      5 * time.Second,
		StatementTimeout: 2 * time.Minute,
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

For a reviewable plan instead of boot sync, use `PlanDiff` then `Apply` with `ExpectedHash`. SQLite works the same way with `modernc.org/sqlite`; see [examples/sqlite-embedded](examples/sqlite-embedded).

## What it manages

**PostgreSQL 14–18** (CI-tested): tables, columns, indexes (`CONCURRENTLY` by default), FKs (`NOT VALID` then `VALIDATE`), CHECKs, enums, domains, extensions, COMMENT ON, functions/procedures/aggregates, triggers, views/matviews, RLS + policies, plus role/grant and publication/event-trigger statements in unified `SchemaSQL`. Optional `--roles` / `RolesSQL` and `--catalog` / `CatalogSQL` files remain supported as authoritative overlays.

**SQLite 3.35+**: tables, columns, indexes, FKs, CHECKs, generated columns, and 12-step rebuilds when in-place ALTER is not enough. Pure Go (`modernc.org/sqlite`) or CGo (`mattn/go-sqlite3`).

Experimental (API may change before 1.0): column renames (`Options.Renames`) and expand/contract (`Options.ExpandContract`, `Options.Backfill`).

Full surface and hazards: [docs/SPEC.md](docs/SPEC.md), [docs/SAFETY.md](docs/SAFETY.md).

## CLI

```bash
grizzle plan  --dsn "$DATABASE_URL" --schema schema.sql --out plan.json
grizzle apply --dsn "$DATABASE_URL" --plan plan.json
grizzle check --dsn "$DATABASE_URL" --schema schema.sql   # exit 4 on drift
grizzle export --plan plan.json --format goose --out ./migrations

# optional side channels
grizzle apply --dsn "$DATABASE_URL" --schema schema.sql --roles roles.sql --catalog catalog.sql
```

| Exit | Meaning |
| :--- | :--- |
| `0` | Success / in sync |
| `1` | Execution or connection error |
| `2` | Unaccepted critical hazard |
| `3` | Plan hash drift on apply |
| `4` | Schema drift on check |

## PgBouncer

Advisory locks are session-scoped. Use session pooling or a direct Postgres connection for migrations. Transaction pooling is not supported (the lock and `SET` timeouts can move to another backend mid-run).

## Examples

- [postgres-stdlib](examples/postgres-stdlib) — `database/sql` + pgx boot sync
- [sqlite-embedded](examples/sqlite-embedded) — pure-Go SQLite
- [sqlc-workflow](examples/sqlc-workflow) — shared `schema.sql` with sqlc
- [postgres-pgxpool](examples/postgres-pgxpool) — `*pgxpool.Pool`
- [kubernetes-blueprint](examples/kubernetes-blueprint) — multi-replica boot
- [expand-contract](examples/expand-contract) — staged rename (experimental)
- [roles-grants](examples/roles-grants) / [catalog-sync](examples/catalog-sync) — role/catalog files and unified-schema overlays

## Docs

- [SPEC](docs/SPEC.md) — API and managed constructs
- [SAFETY](docs/SAFETY.md) — hazards and drop policy
- [ARCHITECTURE](docs/ARCHITECTURE.md) — package layout and runtime flow
- [CONTRIBUTING](CONTRIBUTING.md) — devenv, hooks, PR checklist
- [CHANGELOG](CHANGELOG.md)

Current tag: **v0.1.0-rc2**.

## License

Apache 2.0. See [LICENSE](LICENSE).
