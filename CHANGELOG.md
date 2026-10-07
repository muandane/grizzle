# Changelog

All notable changes to Grizzle will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Plan Operational Fields & Document Round-Trip**:
  - `Plan` carries persisted execution knobs (`LockID`, `LockNamespace`, `LockTimeout`, `StatementTimeout`, `NonConcurrentIndexes`, `ShadowSchema`) so direct `Apply` reproduces the locking and timeout posture used to generate the plan. They are serialized in the plan document envelope (durations as nanoseconds) and excluded from `Plan.Hash()` approval digests.
  - `Plan.ValidateExecutionFields()` validates operational fields on load and apply: `ShadowSchema` must use the reserved `_grizzle_shadow` prefix when set, be ≤ 63 bytes (PostgreSQL `NAMEDATALEN`), be a valid SQL identifier, and never name a target or included schema; `LockNamespace` must be a valid identifier; timeouts must be within `[0, 24h]`. Violations wrap `ErrInvalidOptions`. `ParsePlanJSON` rejects tampered artifacts (e.g. `shadow_schema` rewritten to `"public"`), and `Apply` re-validates before touching the database.
  - `grizzle.ErrHistoryRecord`: typed non-fatal error surfaced when the migration succeeds but the success-path `grizzle_history` record could not be written (detect with `errors.Is`; the schema changes remain applied). Failure-path history write failures are logged and never swallowed or misreported as success.
  - Hook panic recovery: a panicking lifecycle hook is converted into an error carrying the full stack trace (returned to the caller and logged at ERROR); only the first line of the panic message is persisted to `grizzle_history`.
- **Retry Classification (`exec.IsRetryable`)**:
  - Lock-contention retries are classified by PostgreSQL SQLSTATE (`55P03` lock_timeout, `40P01` deadlock) instead of error-string matching, working through wrapped errors and PgBouncer/transaction-pooling proxies.

### Changed
- **`LockTimeout` is a total budget across retries**: the advisory-lock acquisition wait is bounded once and shared across all retry attempts (including backoff waits). Only lock waiting consumes the budget: preamble work (hooks, schema setup, session timeouts) and DDL execution are excluded — each attempt's acquisition timer is armed when acquisition begins, so slow hooks or retryable DDL failures cannot starve the acquisition window. `cfg.LockTimeout` still feeds the per-statement DDL `lock_timeout` via session timeouts. Retryable attempts exit early once waiting the backoff would exhaust the remaining budget.
- **Unique per-call shadow schemas for `PlanDiff`**: each read-only drift check compiles in a unique `_grizzle_shadow_<hash>` schema (≤ 63 bytes), so concurrent `PlanDiff` calls never serialize on shadow DDL locks and cannot collide with a concurrently running `Sync`. Custom `Options.ShadowSchema` values without the reserved prefix are accepted but not persisted into plan artifacts (Apply re-derives the default).
- **SQLite foreign-key pinning**: `PRAGMA foreign_keys` is toggled per-connection on a pinned `*sql.Conn` (with read-back verification) instead of on `*sql.DB`, and `PRAGMA foreign_key_check` runs inside the migration transaction on the pinned connection before commit — safe under transaction-pooling proxies.

### Fixed
- **SQLite foreign keys during migration**: `PRAGMA foreign_keys = OFF` on `*sql.DB` does not affect other pooled connections (and is a no-op inside a transaction); migrations now disable and restore foreign keys on a single pinned connection for the whole transaction.
- **Advisory-lock release hygiene (PostgreSQL)**: lock release and session-state `RESET` run on a detached, bounded context after cancellation, and a failed release discards the connection (`driver.ErrBadConn`) so a session that may still hold advisory locks is never returned to the pool.
- **Dialect detection via driver package path** for registered-but-unmatched driver types (e.g. wrappers around `pgx` or `sqlite`).
- **Timeout semantics in tests**: integration timing assertions loosened to remove CI scheduling flakes; `TestTimeouts_ConflictingHolder_RetrySucceeds` updated to total-budget `LockTimeout` semantics.

### Security
- **Plan artifact validation on load**: untrusted `plan.json` documents are validated (`ValidateExecutionFields`) before use, rejecting shadow schemas that would collide with target schemas (the shadow is dropped with `CASCADE` before compilation).

## [0.1.0] - 2026-10-06

### Added
- **SQLite Table Rebuild Engine Improvements**:
  - **Trigger and View Preservation**: Introspects triggers (`sqlite_schema` where `type='trigger'`) and views (`sqlite_schema` where `type='view'`) referencing target tables, drops views prior to table recreation to prevent SQLite rename errors, and rebinds all views and triggers to the recreated table after renaming.
  - **Chunked Keyset Batch Copying**: Supports keyset pagination (`ORDER BY rowid ASC LIMIT ?` / `WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`) during rebuild data copying for tables exceeding configured size thresholds (`Options.SQLiteRebuildThreshold`, default 100,000; `Options.SQLiteRebuildBatchSize`, default 10,000), preventing SQLite journal memory exhaustion on large tables. Benchmarked across chunk sizes and WAL checkpoint page counts.
  - **Savepoint Isolation & FK Validation**: Encloses each table rebuild execution inside `SAVEPOINT grizzle_rebuild` and validates `PRAGMA foreign_key_check` under the savepoint. Rolls back directly to the savepoint and aborts on any constraint violations before committing the transaction.
  - **Multi-Schema Rejection**: Explicitly rejects multi-schema configurations (`len(TargetSchemas) > 1`) on SQLite with typed `ErrUnsupportedMultiSchema`.
- **Interactive Terminal Inspection**:
  - Interactive TTY confirmation flow for `grizzle apply` without `--plan` when connected to an interactive terminal (`os.Stdin`).
  - Terminal summary table rendering planned changes with operation badges (`+`, `~`, `!`), target tables/columns, and hazard severity markers (`[CODE: LEVEL]`).
  - Detailed critical hazard explanations listing specific data loss risks and consequences prior to confirmation.
  - Interactive prompts (`[y/N/details]`) supporting confirmation, full SQL statement inspection (`details` / `d`), and safe cancellation (`n` / `N` / default empty line) exiting with code 1.
  - Real pseudo-terminal integration tests using `github.com/creack/pty` testing `y`, `N`, and `details` inputs against native TTY descriptors.
  - Preserves strict headless / CI behavior: interactive prompts disabled without TTY, requiring explicit `--accept-hazard <CODE>` and exiting immediately with code 2 on unaccepted critical hazards.
- **Multi-Schema Support (PostgreSQL)**:
  - Declarative management across multiple PostgreSQL schemas via `Options.TargetSchemas` (with `Options.TargetSchema` maintained as a deprecated alias).
  - Isolated per-schema shadow environments (`_grizzle_shadow_<schema>`) and SQL qualifier rewriting (`RewriteShadowSQL`) isolating DDL statements during shadow compilation.
  - Multi-schema catalog introspection and cross-schema foreign key canonical normalization preserving foreign references while stripping local schema qualifiers.
  - Cross-schema foreign key topological sorting (`SortSteps`): referenced tables created before referencing tables; reverse order on drop.
  - Dedicated per-schema 2-integer advisory locks (`pg_try_advisory_lock(hash32(LockNamespace), hash32(schema))`) acquired in sorted, deduplicated order on a single pinned connection, rolling back previously acquired locks on acquisition failure, and releasing in reverse order upon completion.
  - Cross-app isolation configurable via `Options.LockNamespace` (default `"grizzle"`).
- **Partial and Functional Indexes**:
  - Declarative support for PostgreSQL and SQLite functional indexes (indexing expressions like `lower(email)`) and partial indexes (filtered by `WHERE` predicates).
  - Catalog introspection of index predicates via `pg_get_expr(ix.indpred, ix.indrelid)`.
  - Shadow compilation normalization eliminating false diffs caused by PostgreSQL type casts, parentheses wrapping, and schema qualifications on functions/types.
  - Non-immutable function error wrapping: Intercepts shadow schema pre-flight compilation failures and wraps them with index name, table name, and `"expression must be IMMUTABLE"`, asserting target database is untouched.
  - External schema function resolution: Shadow search path includes target schema (`SET LOCAL search_path TO shadow, target, public;`) allowing custom functions in non-table schemas to resolve during shadow DDL compilation.
  - Non-transactional execution (`NonTx=true`) with `CREATE INDEX CONCURRENTLY` and `DROP INDEX CONCURRENTLY`.
  - Automatic detection and concurrent repair of broken indexes (`indisvalid = false`) resulting from aborted index builds.
- **PostgreSQL Partitioned Tables**:
  - Declarative support for range (`RANGE`), list (`LIST`), and hash (`HASH`) partitioned tables, attached partitions (`PARTITION OF ... FOR VALUES ...`), and multi-level sub-partition hierarchies.
  - Catalog introspection of partitioning strategies, partition keys, and partition inheritance bounds from `pg_partitioned_table`, `pg_inherits`, and `pg_class`, filtering out internal index partitions.
  - `DETACH PARTITION CONCURRENTLY`: Renders as standalone `NonTx` step on PostgreSQL 14+, falling back to plain `DETACH` below PG 14, when `--non-concurrent-indexes` is set, or when a default partition exists.
  - Interrupted pending-detach handling: Introspects `pg_inherits.inhdetachpending` and plans `ALTER TABLE ... DETACH PARTITION ... FINALIZE` alongside `PARTITION_PENDING_DETACH` warning hazard.
  - Structural validation: Pure diff checks that `PRIMARY KEY` and unique index definitions on partitioned tables include all partition key columns, rejecting violations at plan time with typed `ErrPartitionKeyNotInUnique`.
  - Wrapped foreign key errors: Wraps runtime foreign key errors on partitioned tables with table and constraint context.
  - Invariant enforcement: strictly rejects in-place conversion between regular tables and partitioned tables (`ErrPartitionConversion`).
  - Emits `PARTITION_ATTACH_SCAN` (`WARNING`) hazard when attaching an existing standalone table to a partitioned table to flag table validation scans under `ACCESS EXCLUSIVE` lock.
  - Safe partition detachment (`DETACH_PARTITION`) to standalone managed tables.
  - Skips direct column alterations on child partitions to ensure schema alterations route cleanly through parent partitioned tables.
- **Unmanaged Object Detection & Protection**:
  - PostgreSQL introspection catalogs unmanaged database entities including views, materialized views, triggers, functions, procedures, external sequences, and unmanaged domains.
  - Structural dependency graph resolved via `pg_depend` and `pg_rewrite`; function and procedure table/column dependencies resolved from `pg_depend` for SQL-standard (`BEGIN ATOMIC`) bodies and via a conservative source scan for quoted string bodies (`LANGUAGE sql AS '...'`, PL/pgSQL).
  - Emits `UNMANAGED_DEPENDENCY` (`CRITICAL`) hazard whenever a planned drop or type alteration impacts an unmanaged object, blocking execution unless explicitly accepted; the hazard description and interactive summary include the affected objects and the remediation path.
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
- **Schema Linting (L001–L007)**:
  - Pure, database-free static analysis of the desired schema IR: `L001` missing primary key (ERROR), `L002` unindexed foreign key columns, `L003` non-snake_case identifiers, `L004` legacy SERIAL/nextval defaults (all WARNING), `L005` non-snake_case CHECK constraint names and `L006` duplicate CHECK expressions (WARNING), `L007` auto-generated CHECK names (INFO).
  - Severity levels `ERROR` / `WARNING` / `INFO`; INFO findings render as GitHub `::notice` annotations.
  - Formatters for human text with severity counts, stable JSON, and GitHub Actions workflow annotations; custom rules implement `LintRule` and mix with `DefaultLintRules`.
  - CLI `grizzle lint --schema schema.sql` with `--format text|json|github` and `--fail-on-warning` (fails on WARNING severity, not INFO).
- **Dry-Run Verification**:
  - `Options.DryRun` plans and executes the migration against live data inside a rolled-back sandbox, catching data-dependent failures (constraint violations, invalid data casts) before any DDL persists.
  - CLI `grizzle apply --dry-run`; bounded lock waits via `Options.DryRunLockTimeout`.
- **Lifecycle Hooks**:
  - `Options.BeforeSync` / `Options.AfterSync` run once around the whole migration; `Options.BeforeStep` / `Options.AfterStep` wrap each plan step with bound `HookContext` database access.
  - Hook failures abort before/after the corresponding operation with typed errors; `Options.ExecuteHooksInDryRun` controls hook execution inside dry-run sandboxes.
- **Idempotent Seeding**:
  - `Options.SeedSQL` executes idempotent data-seed SQL after a successful sync (DDL → `AfterSync` → seed); records history and skips already-applied seeds unless `SeedForce` is set.
  - CLI `grizzle seed --seed seed.sql [--force]`.
- **Lock Acquisition Retry**:
  - `Options.MaxRetries` (default 3) with exponential backoff and deterministic jitter (`Options.RandFloat` injection point for tests); retries halt immediately once any DDL step commits.
- **Declarative CHECK Constraints (PostgreSQL)**:
  - Named and inline `CHECK` table constraints introspected (`pg_constraint.contype = 'c'`, local constraints only), diffed, and managed end-to-end.
  - Staged safely like foreign keys: `ADD CONSTRAINT ... NOT VALID` in one transaction group, then `VALIDATE CONSTRAINT` in a separate group to keep `SHARE UPDATE EXCLUSIVE` scans off the exclusive lock window (`CHECK_VALIDATE_SCAN` notice).
  - Redefinitions emitted as drop + re-add; constraint drops guarded by `Options.AllowDropCheck` and the `DROP_CHECK` notice hazard.
  - Constraints auto-named by PostgreSQL (inline `CHECK` syntax) are validated and redefined but never auto-dropped; explicitly named constraints are fully managed.
  - SQLite check constraints remain out of scope (not introspected; not preserved through rebuilds).
- **Staged Expand-and-Contract Migrations**:
  - Staged non-destructive migrations enabled via experimental `Options.ExpandContract`.
  - Plan 1 adds new columns as nullable alongside existing columns.
  - Plan 2 (contract phase) is emitted separately with its own deterministic approval hash to drop legacy columns.
  - Added `BackfillFunc` hook (`Options.Backfill` / `ApplyOpts.Backfill`) executed in batches outside the DDL lock window.
  - CLI support: repeatable `--rename old=new` (table-qualified `table.old=new` supported) and `--expand-contract` on `plan` / `apply` / `check` / `--dry-run`; backfill remains library-only.
  - Runnable end-to-end example in `examples/expand-contract`.
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
