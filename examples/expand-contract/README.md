# Zero-downtime expand/contract migration

## Overview

Demonstrates a column rename (`users.full_name` → `users.display_name`) executed without downtime using Grizzle's staged expand-and-contract flow (experimental):

| Phase | What happens | Approval |
| :--- | :--- | :--- |
| 1. Expand | `display_name` is added alongside `full_name` as nullable — no existing column or data is touched | Safe; non-destructive |
| 2. Backfill | Existing values are copied in batches **outside** the DDL lock window via the library-only `Options.Backfill` hook | Library code (`WithBackfill`) |
| 3. Contract | A second, separately-approved plan drops `full_name` once the application only reads `display_name` | Requires `AllowDropColumn` + `AcceptHazards: [DROP_COLUMN]` |

The old column stays fully functional during the expand and backfill phases, so rolling deploys and long-running queries are unaffected.

## Usage

```bash
export DATABASE_URL="postgres://postgres:password@localhost:5432/myapp?sslmode=disable"
export PG_SCHEMA="zdm_demo"   # optional schema scoping
go run .
```

The example is idempotent: on an empty database it bootstraps the v1 shape, then runs expand → backfill → contract.

## CLI equivalents

Planning the same flow with the CLI (backfill stays library-only):

```bash
# Phase 1: expand plan (adds display_name alongside full_name)
grizzle plan --schema schema.sql --out expand.json \
  --rename users.full_name=display_name --expand-contract

# Phase 3: contract plan (drops full_name once clients are migrated)
grizzle plan --schema schema.sql --out contract.json \
  --rename users.full_name=display_name --allow-drop
grizzle apply --plan contract.json --accept-hazard DROP_COLUMN
```

> [!NOTE]
> `Options.Renames`, `ExpandContract`, and `Backfill` are **Experimental**: the mapping format, staged plan shape, and backfill batching semantics may change before 1.0. There is no CLI backfill runner; backfill is library-only.

## Operational sequence

1. Deploy the expand plan with application version N (still writing `full_name`).
2. Ship application version N+1 that writes **both** columns.
3. Run the backfill (the `Options.Backfill` hook runs automatically during expand sync).
4. Deploy the contract plan with application version N+2 (only writing `display_name`).
5. Contract drops `full_name`; any stale reader fails loudly rather than silently losing data.

## Files

- `schema.sql` — the desired end state (v2)
- `main.go` — bootstrap, expand+backfill, contract
- `main_test.go` — integration test against a scratch schema