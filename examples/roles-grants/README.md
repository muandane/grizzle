# Roles and grants (`RolesSQL`)

## Overview

Shows the optional `--roles` / `Options.RolesSQL` side-channel: schema DDL stays in `schema.sql`, while `CREATE ROLE` and `GRANT` statements live in `roles.sql` and are diffed against live ACLs after schema sync.

## Files

| File | Role |
| :--- | :--- |
| `schema.sql` | Tables / functions that grants reference (`docs`, `notify_event`) |
| `roles.sql` | Desired roles and privileges |
| `main.go` | In-process `Sync` with `SchemaSQL` + `RolesSQL` |

## Running

```bash
devenv up
go run .                       # or: DATABASE_URL=... PG_SCHEMA=my_schema go run .

# CLI equivalent
grizzle apply --dsn "$DATABASE_URL" --schema schema.sql --roles roles.sql

go test -tags integration ./...
```

Uncomment and rename the `GRANT CONNECT ON DATABASE …` line in `roles.sql` if you want database-level grants (cluster-scoped; must match the DSN database name).

See [docs/SPEC.md](../../docs/SPEC.md) §2.2 (RolesSQL contract) and [examples/catalog-sync](../catalog-sync) for the catalog overlay.
