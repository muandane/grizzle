# Catalog sync (`CatalogSQL`)

## Overview

Shows the optional `--catalog` / `Options.CatalogSQL` side-channel for publications and event triggers. Schema DDL (including the event-trigger function) stays in `schema.sql`; catalog objects live in `catalog.sql` and are never shadow-compiled.

## Files

| File | Role |
| :--- | :--- |
| `schema.sql` | `docs` table + `log_ddl()` event-trigger function |
| `catalog.sql` | `CREATE PUBLICATION` / `CREATE EVENT TRIGGER` desired state |

## Running

```bash
# Requires a role that can create publications; event triggers usually need superuser.
grizzle apply --dsn "$DATABASE_URL" --schema schema.sql --catalog catalog.sql

# Plan only
grizzle plan --dsn "$DATABASE_URL" --schema schema.sql --catalog catalog.sql --out plan.json
```

`CREATE EVENT TRIGGER` steps carry an `EVENT_TRIGGER_SUPERUSER` warning. Operator-created publications/triggers are never swept — only `grizzle-managed` objects that leave `catalog.sql` are dropped, behind `AllowDropPublication` / `AllowDropEventTrigger` plus the matching critical hazards.

See [docs/SPEC.md](../../docs/SPEC.md) §2.2 (CatalogSQL contract), [examples/roles-grants](../roles-grants) for the roles overlay, and [examples/full-catalog](../full-catalog) for a single-file schema surface.
