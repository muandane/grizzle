# Changelog

All notable changes to Grizzle will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-10-06

### Added
- **Partial and Functional Indexes**:
  - Declarative support for PostgreSQL and SQLite functional indexes (indexing expressions like `lower(email)`) and partial indexes (filtered by `WHERE` predicates).
  - Catalog introspection of index predicates via `pg_get_expr(ix.indpred, ix.indrelid)`.
  - Shadow compilation normalization eliminating false diffs caused by PostgreSQL type casts, parentheses wrapping, and schema qualifications on functions/types.
  - Non-transactional execution (`NonTx=true`) with `CREATE INDEX CONCURRENTLY` and `DROP INDEX CONCURRENTLY`.
  - Automatic detection and concurrent repair of broken indexes (`indisvalid = false`) resulting from aborted index builds.
- **PostgreSQL Partitioned Tables**:
  - Declarative support for range (`RANGE`), list (`LIST`), and hash (`HASH`) partitioned tables and attached partitions (`PARTITION OF ... FOR VALUES ...`).
  - Catalog introspection of partitioning strategies, partition keys, and partition inheritance bounds from `pg_partitioned_table`, `pg_inherits`, and `pg_class`.
  - Invariant enforcement: strictly rejects in-place conversion between regular tables and partitioned tables (`ErrPartitionConversion`).
  - Emits `PARTITION_ATTACH_SCAN` (`WARNING`) hazard when attaching an existing standalone table to a partitioned table to flag table validation scans under `ACCESS EXCLUSIVE` lock.
  - Safe partition detachment (`DETACH_PARTITION`) to standalone managed tables.
  - Skips direct column alterations on child partitions to ensure schema alterations route cleanly through parent partitioned tables.
- **Unmanaged Object Detection & Protection**:
  - PostgreSQL introspection catalogs unmanaged database entities including views, materialized views, triggers, functions, procedures, external sequences, and unmanaged domains.
  - Structural dependency graph resolved via `pg_depend` and `pg_rewrite`.
  - Emits `UNMANAGED_DEPENDENCY` (`CRITICAL`) hazard whenever a planned drop or type alteration impacts an unmanaged object, blocking execution unless explicitly accepted.
- **Generated Column Support**:
  - Full support for generated columns on PostgreSQL (`GENERATED ALWAYS AS (...) STORED`) and SQLite (`VIRTUAL` and `STORED` via `table_xinfo`).
  - Expression normalization (`NormalizeGeneratedExpr`) eliminates false schema diffs across database versions.
  - Emits `GENERATED_REWRITE` (`WARNING`) hazard on expression modifications to flag full table rewrites on PostgreSQL.
  - Safe 12-step rebuild integration for SQLite generated columns.
- **CLI Tool (`cmd/grizzle`)**:
  - `grizzle plan --out plan.json`: Serializes deterministic migration plan documents with step details, hazards, hash, and options digest.
  - `grizzle apply --plan plan.json [--accept-hazard CODE ...]`: Executes migration plans with strict plan-drift validation against live schema.
  - `grizzle check`: Validates database schema sync state for CI pipelines.
  - Deterministic exit codes:
    - `0`: Success / schema in sync / no-op.
    - `1`: Execution or connection error.
    - `2`: Blocked by unaccepted hazard.
    - `3`: Plan hash drift during `apply` (`ErrPlanDrift`).
    - `4`: Schema drift detected during `check`.
  - Credentials redaction across all structured `slog` logs and console output.
- **Migration Plan Export (`grizzle export`)**:
  - Pure export engine supporting three formats:
    - `sql`: Raw ordered SQL with explicit `-- grizzle:notx` annotations for non-transactional steps (`CONCURRENTLY`).
    - `goose`: Goose-formatted migrations with `-- +goose Up`, `-- +goose NO TRANSACTION`, and reversible `-- +goose Down` blocks (or clear comments when irreversible).
    - `atlas`: Plain versioned SQL migration files with plan hash headers.
- **Session-Level Locking for Concurrent Operations**:
  - Multi-pod migration lock coordination using dedicated session-level advisory locks (`pg_advisory_lock` / `pg_try_advisory_lock`) maintained across both transactional and non-transactional DDL groups.
  - Non-blocking lock acquisition polling loop with client-side `context.WithTimeout` enforcement, preventing server-side wait queue deadlocks against concurrent DDL.
  - Automatic connection hygiene resets (`RESET search_path; RESET lock_timeout; RESET statement_timeout;`) upon connection return to the connection pool.
  - Direct plan `Apply` execution parity via `exec.ApplyPostgres` and `exec.ApplySQLite` ensuring dedicated session advisory locking and step grouping for pre-approved plans without `SchemaSQL`.
  - Multi-pod mutual exclusion idempotency via `history.IsApplied` under the advisory lock to gracefully skip duplicate execution across competing replicas.
- **Automated Invalid Index Recovery**:
  - PostgreSQL schema inspection checks `pg_index.indisvalid`.
  - Broken indexes left by failed `CREATE INDEX CONCURRENTLY` executions are automatically detected and repaired via `DROP INDEX CONCURRENTLY` and clean recreation.
- **Staged Expand-and-Contract Migrations**:
  - Staged non-destructive migrations enabled via experimental `Options.ExpandContract`.
  - Plan 1 adds new columns as nullable alongside existing columns.
  - Plan 2 (contract phase) is emitted separately with its own deterministic approval hash to drop legacy columns.
  - Added `BackfillFunc` hook (`Options.Backfill` / `ApplyOpts.Backfill`) executed in batches outside the DDL lock window.
- **Audit History**:
  - `grizzle_history` tracks execution lifecycle: `status` (`applied`, `partial`, `failed`), `failed_step`, `error`, and `plan_hash`.
  - Failure records are guaranteed via dedicated connections and detached contexts even if DDL transactions abort.
- **SQLite Engine Parity**:
  - Parity for column renames, hazard gates (`HazardTypeNarrow`, `HazardRenameAmbiguous`, `HazardNotNullNoDefault`), and `StrictScope` enforcement.

### Changed
- **Package Restructuring**:
  - Layered architecture with strict one-way dependency rules: `schema <- scope, diff, plan <- dialect <- exec, history <- grizzle (root)`.
  - Core layers (`schema`, `scope`, `diff`, `plan`, `export`) are pure with zero I/O, `database/sql`, or context dependencies.
  - All public types, facades (`Sync`, `PlanDiff`, `Apply`, `Check`, `Export`), and constructors exported from root.
- **Hazard Gating Behavior**:
  - Critical hazards (`DROP_TABLE`, `DROP_COLUMN`, `TYPE_NARROW`, `RENAME_AMBIGUOUS`, `NOT_NULL_NO_DEFAULT`, `UNMANAGED_DEPENDENCY`) cause `Apply` to fail with `ErrHazardBlocked` by default unless explicitly provided in `AcceptHazards`.
- **Constraint Validation Staging**:
  - Foreign key and check constraint `VALIDATE CONSTRAINT` statements execute in a separate transaction group following `ADD ... NOT VALID` commits, releasing `ACCESS EXCLUSIVE` table locks prior to full table validation scans.

### Removed
- **`Options.ExpectedHash`**:
  - **Breaking Change**: Removed `ExpectedHash` and `WithExpectedHash` from `Options`. Approval hashes must now be passed explicitly via `ApplyOpts.ExpectedHash` in `grizzle.Apply(ctx, db, plan, grizzle.ApplyOpts{ExpectedHash: ...})`.
- **Module Path Update**:
  - **Breaking Change**: Module path migrated from `github.com/yourorg/grizzle` to `github.com/muandane/grizzle`.

### Fixed
- **Retry Safety**:
  - Lock acquisition retries (PostgreSQL SQLSTATE `55P03`) are halted immediately once any DDL step commits, preventing duplicate execution of partial migrations.
- **Zero-Value Options Safety**:
  - Calling `Sync`, `PlanDiff`, or `Check` with an empty or unconfigured `Options{}` fails gracefully with `ErrEmptySchema` without panic or unintended DDL side effects.

### Security
- **Strict Default-Deny Drops**:
  - Destructive operations (`DROP TABLE`, `DROP COLUMN`) require explicit opt-in (`AllowDrop: true` and `AcceptHazards`).
- **Connection Credential Masking**:
  - CLI logs and error messages strip authentication credentials and passwords from database URIs.
