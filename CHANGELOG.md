# Changelog

All notable changes to Grizzle will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v0.4.0] - 2026-10-07

### Added
- **CatalogSQL sync** (`feat(plan): CatalogSQL sync`): new side-channel contract for cluster-catalog objects (PostgreSQL only; CLI `--catalog catalog.sql`, library `Options.CatalogSQL`). Statement-scanned `CREATE PUBLICATION` / `CREATE EVENT TRIGGER` statements are diffed against live `pg_publication` / `pg_event_trigger` catalogs and applied after roles and grants. Publications diff on membership (`FOR TABLE`, `FOR ALL TABLES`, `FOR TABLES IN SCHEMA` — PostgreSQL 15+ for schema-level) and `publish` flags (absent clause defaults to all four DML operations); drift renders `ALTER PUBLICATION`. Event triggers diff on event, `WHEN TAG IN` filter, function, and enabled state — definition drift is DROP+CREATE (no in-place ALTER), enabled-only drift renders `ALTER EVENT TRIGGER ENABLE/DISABLE`. The sync aborts up front when an event-trigger function is missing. Drops are narrow: only objects stamped with the `grizzle-managed` catalog comment are considered, gated behind `AllowDropPublication` / `AllowDropEventTrigger` + `DROP_PUBLICATION` / `DROP_EVENT_TRIGGER` (CRITICAL). `EVENT_TRIGGER_SUPERUSER` (WARNING) fires on every event-trigger step; `PUBLICATION_ALL_TABLES` (NOTICE) on FOR ALL TABLES breadth. `CatalogSQL` participates in `Plan.Hash()` when non-empty; SQLite + non-empty `CatalogSQL` is rejected (`ErrInvalidOptions`). New example: `examples/catalog-sync/`.

## [v0.3.0] - 2026-10-07
### Added
- **RolesSQL privilege sync** (`feat(plan): RolesSQL privilege sync`): new side-channel contract for grants/roles (PostgreSQL only; CLI `--roles roles.sql`, library `Options.RolesSQL`). Statement-scanned `CREATE ROLE`/`GRANT` statements are diffed against live `pg_authid` roles and object ACLs (`aclexplode`) and applied after all schema DDL. Managed roles are always `NOLOGIN` and stamped with a `grizzle-managed` catalog comment — only marker-stamped roles are dropped, and only behind `AllowDropRole` + `DROP_ROLE` (CRITICAL), with an ownership refusal when the role owns cluster objects. Missing grants become `GRANT`; surplus grants become `REVOKE` behind `AllowRevoke` + `REVOKE_PRIVILEGE` (CRITICAL); grant-option drift renders `WITH GRANT OPTION` / `REVOKE GRANT OPTION FOR`. Grants to `PUBLIC` or to roles Grizzle does not manage are never revoked; a desired `GRANT ... TO PUBLIC` emits `GRANT_PUBLIC` (WARNING). Unqualified `TABLE`/`SEQUENCE` objects resolve against the primary target schema. `RolesSQL` participates in `Plan.Hash()` when non-empty; SQLite + non-empty `RolesSQL` is rejected (`ErrInvalidOptions`). New `Options.AllowRevoke` / `Options.AllowDropRole` fields (inheriting from `AllowDrop`) round-trip through the plan artifact so policy stays approval-consistent across the post-lock re-diff. New example: `examples/roles-grants/roles.sql`.

## [v0.2.0] - 2026-10-07

### Breaking
- **Plan hash format**: `Plan.Hash()` now includes `DropPolicy`, `NonConcurrentIndexes`, and `SchemaSQL` (approval-sensitive intent). Previously approved plan artifacts will fail `ExpectedHash` / envelope verification (`ErrPlanDrift`).
- **Plan artifact no longer persists lock/shadow identity**: `lock_id`, `lock_namespace`, and `shadow_schema` are removed from the plan document. Lock identity is derived at apply time from trusted target identity + runtime `ApplyOpts.LockNamespace`; shadow schemas are generated ephemerally per run.
- **`options_digest` removed** from the plan document envelope (it was non-authoritative decorative metadata).
- **`GENERATED_REWRITE` is CRITICAL**: generated-column rewrites (drop/recreate) require explicit `AcceptHazards: GENERATED_REWRITE`.
- **Managed-surface objects leave the protected path**: extensions, RLS flags, policies, functions, triggers, and views/matviews declared in `SchemaSQL` are now diffed and synced declaratively. Plans that previously reported "nothing to do" for drifted views/triggers/functions now emit managed steps; live-only objects become destructive drop steps gated by the new `AllowDrop*` flags and hazard codes.

### Added
- **Managed COMMENT ON** (`feat(diff): manage COMMENT ON`): `Comment` on tables and columns is introspected from `obj_description` / `col_description` and diffed declaratively. Set/clear is non-destructive (`COMMENT ON … IS '…'` / `IS NULL`); clearing a non-empty live comment emits `COMMENT_CLEAR` (NOTICE). Reversible in exports (previous comment restored). SQLite ignores comments.
- **Managed procedures** (`feat(diff): manage procedures and aggregates`): procedures (`prokind = 'p'`) are managed like functions — `pg_get_functiondef` is canonical; body drift → `CREATE OR REPLACE PROCEDURE`; signature drift → DROP+CREATE; drops share `AllowDropFunction` + `DROP_FUNCTION`. Window functions (`prokind = 'w'`) remain protected as unmanaged dependencies.
- **Managed aggregates** (`feat(diff): manage procedures and aggregates`): aggregates (`prokind = 'a'`, default `aggkind`) get a canonical definition reconstructed from `pg_aggregate` catalog fields (`pg_get_functiondef` does not support aggregates). Any drift is DROP+CREATE (`CREATE_AGGREGATE`/`DROP_AGGREGATE` plan steps; no `CREATE OR REPLACE AGGREGATE`); steps sort after support-function creates and before their drops. Drops share the `AllowDropFunction` gate + `DROP_FUNCTION` hazard. Ordered-set/hypothetical aggregates remain protected.
- **Managed domains** (`feat(diff): manage domains`): PostgreSQL domains (`CREATE DOMAIN`) leave the protected path. `Schema.Domains` diffs base type, nullability, default, and CHECK constraints (`pg_constraint.conrelid = 0`) between live and shadow catalogs. Base type/nullability/default drift is DROP+CREATE (`DROP_DOMAIN_RETYPE` then `CREATE_DOMAIN` — `ALTER DOMAIN` cannot retype); CHECK drift is `ALTER DOMAIN ADD/DROP CONSTRAINT` (adds non-destructive; removals/redefinitions gated). Live-only domain drops are gated by `AllowDropDomain` + `DROP_DOMAIN` (CRITICAL) and run after table drops; no implicit `CASCADE`, so domains with dependent columns fail at apply instead of destroying them. Extension-owned domains remain excluded.
- **SQLite CHECK constraints** (`feat(sqlite): manage CHECK constraints`): named and inline CHECK constraints are parsed from `sqlite_schema.sql` into `Table.Checks`, emitted as table-level `CONSTRAINT` lines on create/rebuild (so rebuilds preserve them), and diffed — check drift triggers a table rebuild; removals are destructive and gated by `AllowDropCheck` + `DROP_CHECK`. Added checks rebuild non-destructively (`ADD_CHECK`). No `NOT VALID`/`VALIDATE` path exists on SQLite, so no `CHECK_VALIDATE_SCAN` is emitted.
- **Managed extensions** (`feat(schema): declarative extension management`): `CREATE EXTENSION` statements in `SchemaSQL` are statement-scanned into `Schema.Extensions`, stripped from shadow DDL, and best-effort re-installed in the shadow transaction so extension-provided types (e.g. `citext`) compile. Diff creates missing extensions; live-only extensions are dropped only with `AllowDropExtension` + `DROP_EXTENSION`. `EXTENSION_PRIVILEGE` (WARNING) fires on create.
- **Managed RLS + policies** (`feat(diff): manage RLS and policies`): `Table.RLSEnabled` / `RLSForced` flags and `Table.Policies` are introspected from `pg_class` / `pg_policy` (expressions normalized). Flag drift emits `ENABLE/DISABLE/FORCE ROW LEVEL SECURITY`; policy drift emits `CREATE POLICY` or DROP+CREATE (gated by `AllowDropPolicy` + `DROP_POLICY`). `RLS_ENABLE` (WARNING) fires when enabling/forcing.
- **Managed functions** (`feat(diff): manage functions`): `Schema.Routines` compares canonical `pg_get_functiondef` output with search-path-independent inspection. Body drift → `CREATE OR REPLACE FUNCTION`; identity-args/return-type/kind/language drift → DROP+CREATE; live-only routines drop with `AllowDropFunction` + `DROP_FUNCTION`. `SECURITY_DEFINER` (WARNING) fires for definer routines without explicit `search_path`.
- **Managed triggers** (`feat(diff): manage triggers`): `Table.Triggers` compares canonical `pg_get_triggerdef` output; drift is DROP+CREATE, gated by `AllowDropTrigger` + `DROP_TRIGGER`. Surviving managed triggers now count as dependencies for column drops (`UNMANAGED_DEPENDENCY`), replacing the old unmanaged-trigger registration.
- **Managed views / materialized views** (`feat(diff): manage views`): `Schema.Views` compares canonical `pg_get_viewdef` output plus column lists. Append-only column growth replaces in place (`CREATE OR REPLACE VIEW`); anything else (and every matview change) is DROP+CREATE; matview creates emit `REFRESH MATERIALIZED VIEW`. Drops gated by `AllowDropView` + `DROP_VIEW`. Views no longer register as unmanaged dependencies — they are fully managed.
- **Lint L008** (`RLSEnableWithoutPolicies`): WARNING for tables with RLS enabled but zero policies.
- **Lint L009** (`NoDMLStatements`): ERROR for DML (`INSERT`/`UPDATE`/`DELETE`/`TRUNCATE`) declared in `SchemaSQL` — such statements are silently ignored by automigrations; seed data belongs in `SeedSQL`.
- **Export reversals for the new step types**: goose `Down` sections now reverse policy/RLS/function/trigger/view creation; extension creation is deliberately irreversible (Down omitted with an explanatory note).
- **`DryRunVerifyPlan`**: library API that dry-runs an approved plan artifact, verifying the envelope hash and re-diffing against live state so tampered target/policy/step semantics cannot silently verify a different migration. CLI `apply --dry-run --plan` is a thin adapter.
- **`grizzle.ErrHistoryRecord`**: typed non-fatal error when the migration succeeds but the success-path `grizzle_history` record could not be written (detect with `errors.Is`; schema changes remain applied).
- Hook panic recovery: a panicking lifecycle hook is converted into an error carrying the full stack trace (returned to the caller and logged at ERROR); only the first line of the panic message is persisted to `grizzle_history`.
- **Retry Classification (`exec.IsRetryable`)**: lock-contention retries classified by PostgreSQL SQLSTATE (`55P03`, `40P01`).

### Changed
- **Advisory history (Model B)**: `RecordPlan` always runs after DDL commit on a dedicated connection — never inside the migration transaction — so history failure cannot roll back applied DDL.
- **`SortSteps`**: CREATE/DROP table phases use Kahn topological ordering with deterministic lexical SCC collapse for cycles (Warshall transitive closure unchanged).
- **`Options.Validate()`** is the canonical pure validation entry point (identifiers, timeouts, retries, SQLite rebuild params, multi-schema SQLite rejection); `prepareOptions` = Validate + dialect detection + defaults.
- **Ephemeral shadow schemas** for Sync and PlanDiff: unique per-run names via `uniqueShadowName` + bounded `ComputeShadowSchemas` (≤ 63 bytes, collision-safe).
- **`LockTimeout` total budget across retries** (unchanged semantics from prior unreleased work).
- **SQLite foreign-key pinning** on a single connection for the migration transaction.

### Fixed
- **SQLite `INTEGER PRIMARY KEY` vs `AUTOINCREMENT`**: rebuilds preserve exact semantics (no longer force `AUTOINCREMENT` on every single-column integer PK).
- **SQLite `WITHOUT ROWID` rebuilds**: large-table keyset batching never emits `rowid` predicates for `WITHOUT ROWID` tables (falls back to monolithic copy).
- **Advisory-lock release hygiene (PostgreSQL)** and dialect detection via driver package path.

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
