# Full managed catalog: extensions, RLS, functions, triggers, views

## Overview

Demonstrates every managed construct in a single `schema.sql`:

| Construct | What Grizzle does |
| :--- | :--- |
| `CREATE EXTENSION pgcrypto` | Statement-scanned (extensions roll back with the shadow tx); re-installed best-effort in the shadow so `digest(...)` compiles; synced idempotently against `pg_extension` |
| `docs` table | Standard declarative table sync |
| `touch_ts()` + `docs_touch` trigger | Managed via canonical `pg_get_functiondef` / `pg_get_triggerdef`; drift replaces in place or DROP+CREATE |
| `doc_count()` | SQL function managed the same way (`BEGIN`-free bodies compare canonically) |
| `ENABLE ROW LEVEL SECURITY` + policy | Flags diffed from `pg_class`; policy lifecycle via `pg_policy` (replace is DROP+CREATE) |
| `docs_titles` view | `CREATE OR REPLACE VIEW` when columns only grow; DROP+CREATE otherwise |
| `docs_summary` matview | Always DROP+CREATE plus `REFRESH MATERIALIZED VIEW` |

## Safety gates that apply

- `AllowDrop: false` (default) blocks every destructive step, including the new `DROP_POLICY` / `DROP_FUNCTION` / `DROP_TRIGGER` / `DROP_VIEW` gates.
- Removing an object from `schema.sql` plans its drop; applying requires the matching `AllowDrop*` option plus `AcceptHazards` for the CRITICAL hazard.
- `grizzle lint` runs L001–L009; L008 would warn if RLS were enabled with zero policies, L009 rejects DML in `SchemaSQL` (seeds belong in `SeedSQL`).

## Running

```bash
# With devenv (starts a local PostgreSQL on 5432)
devenv up
go run .                       # or: DATABASE_URL=... PG_SCHEMA=my_schema go run .

# Integration test (drops a scratch schema afterwards)
go test -tags integration ./...
```

## Try the gates

```bash
# Plan shows extension/RLS/function/trigger/view steps plus WARNING hazards
grizzle plan --dsn "$DATABASE_URL" --schema schema.sql

# Export a goose migration — Down is omitted because CREATE EXTENSION is irreversible
grizzle export --dsn "$DATABASE_URL" --schema schema.sql --format goose

# Dry-run verifies every step against live data and rolls back
grizzle apply --dsn "$DATABASE_URL" --schema schema.sql --plan plan.json --expected-hash <hash> --dry-run
```

See [examples/postgres-stdlib](../postgres-stdlib) for the minimal table-only variant and [docs/SPEC.md](../../docs/SPEC.md) §3 for the full construct matrix.
