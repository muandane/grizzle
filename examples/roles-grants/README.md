# Roles and grants (`RolesSQL`)

## Overview

Shows the optional `--roles` / `Options.RolesSQL` file: schema DDL stays in `schema.sql`, while `CREATE ROLE` and `GRANT` statements live in `roles.sql` and are diffed against live ACLs after schema sync.

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

`roles.sql` uses placeholder names `public` (schema) and `app` (database) for the CLI default. The example `main.go` rewrites those two grants to the connected `PG_SCHEMA` / `current_database()` before calling `Sync`, because Grizzle rejects `SCHEMA`/`DATABASE` grants outside the current target.

See [docs/SPEC.md](../../docs/SPEC.md) §2.2 (RolesSQL contract) and [examples/catalog-sync](../catalog-sync) for publications and event triggers.
