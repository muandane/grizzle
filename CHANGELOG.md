# Changelog

All notable changes to Grizzle will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.2.0] - 2026-10-14

### Added
- **External Dev Database URL for Shadow Compilation** (`--dev-url` / `Options.DevDatabaseURL`):
  - Support compiling shadow schemas against an external development database instead of the live target database.
  - Actionable error guidance: when the target database user lacks `CREATE SCHEMA` privileges and no dev URL is provided, returns an explicit error naming `--dev-url` / `Options.DevDatabaseURL` and `GRANT CREATE ON DATABASE` with zero silent fallback.
  - CLI integration across `plan`, `apply`, `check`, `lint`, `export`, `status`, and `baseline` via `--dev-url` flag, `GRIZZLE_DEV_URL` / `DEV_DATABASE_URL` environment variables, and `dev_url` config property in `grizzle.yaml`.
- **Composite Primary Key Keyset Backfill** (`internal/exec/backfill.go`):
  - Keyset pagination with SQL row-value comparison `(pk1, ..., pkN) > ($1, ..., $N)` and tuple `IN` updates supporting $N$-column composite primary keys ($N \ge 1$) across PostgreSQL and SQLite.
  - Index verification: requires and verifies that a database index exists whose leading columns match the primary key column order before keyset traversal begins.
  - Mid-run resumption: seamlessly resumes backfill progression from partially-backfilled batches without skipping or duplicating rows.
- **Multi-Tenant Schema Patterns** (`Options.TargetSchemaPattern` / `--target-schema-pattern` / `--schema-pattern`):
  - Dynamic discovery of PostgreSQL schemas via `pg_namespace` matching LIKE patterns (e.g. `tenant_%`), sorted in deterministic ascending order.
  - Automatic exclusion of PostgreSQL system schemas (`pg_%`, `information_schema`) and Grizzle shadow/internal schemas (`_grizzle%`).
  - Plan drift detection: discovered schema list populates `Plan.TargetSchemas` and participates in `Plan.Hash()`, detecting schema additions and removals.
- **Lock Contention Pre-Flight** (`Options.FailOnContention` / `--fail-on-contention` / `LockContentionError`):
  - Pre-flight lock contention inspection before executing DDL steps requiring strong table locks (`AccessExclusiveLock`, `ShareUpdateExclusiveLock`, etc.), querying `pg_locks` joined with `pg_stat_activity` on target tables while filtering out the current backend PID.
  - Advisory mode by default: logs detailed warnings identifying conflicting PIDs, usernames, applications, lock modes, running queries, and durations without gating execution.
  - Fail-on-contention gating via `--fail-on-contention` / `Options.FailOnContention`: retries with exponential backoff up to `LockTimeout`, aborting execution with structured `LockContentionError` (wrapping `ErrLockContention`) if contention does not clear within the timeout.
- **Metrics & Readiness Probe** (`otelgrizzle`, `grizzle.Metrics`, `grizzle.IsReady`):
  - Standardized OpenTelemetry metrics in separate subpackage `otelgrizzle` (`otelgrizzle.NewMetrics`, `otelgrizzle.WithMetrics`), keeping core Grizzle 100% dependency-free:
    - `migration_duration_seconds` (Float64Histogram): end-to-end migration execution duration labeled by `dialect` and `status` (`applied`, `failed`, `noop`).
    - `drift_detected` (Int64Counter): total count of detected schema drift and approved plan hash mismatch events.
    - `lock_wait_seconds` (Float64Histogram): time spent acquiring or waiting for database locks in seconds.
  - Zero-side-effect, lock-free readiness probe `grizzle.IsReady(ctx, db, ...)`:
    - Inspects latest migration state in `grizzle_history` without acquiring advisory locks, creating tables, or mutating state.
    - Returns `(true, nil)` when latest migration record is `"applied"` or `"baseline"`.
    - Returns `(false, nil)` when unmigrated (no `grizzle_history` table or 0 rows) or when latest status is non-ready (`"failed"`, `"partial"`, etc.).
    - Returns `(false, err)` on database connectivity/query failure.
    - Multi-schema support: verifies readiness across all configured target schemas.
  - Zero `Plan.Hash()` impact; 100% backward compatible with existing stored plans.
- **Nix-Native Test Scenarios & Stress Framework** (`devenv.nix`, `test/scenarios/`):
  - Parallel developer deployment race condition scenario (`TestScenario_ParallelDeveloperRace`) testing concurrent migrations under advisory locking, validating deterministic `ErrPlanDrift` rejection and zero dirty state.
  - Connection chaos recovery scenario (`TestScenario_ChaosRecovery`) verifying PostgreSQL transactional rollback and zero orphan table leakage during mid-flight termination.
  - Lock contention stress scenario (`TestScenario_LockContentionStress`) testing backoff and retry under simulated long-running queries.
  - Registered commands in `devenv`: `scenario-race`, `scenario-contention`, `scenario-bench`, `scenario-chaos`, and `scenarios`.
- **Zero-Waste Hot-Path Engine Optimizations**:
  - Keyset backfill buffer reuse: hoisted scan pointers outside the row cursor loop and built pre-allocated parameter builders, reducing allocations by 53% and boosting throughput to 51,644 rows/sec.
  - SQLite schema inspection and diff speedup: 13% faster diff generation with zero-allocation substring pre-filters for check constraint scanning and contiguous table slices.
  - Zero-allocation topological step sorting: added early return and lazy dependency graph allocation for plans with independent DDL operations.
  - Buffered SHA-256 plan hash streaming: implemented `io.StringWriter` buffering and stack number formatting, eliminating heap escapes while guaranteeing 100% byte-for-byte identical golden digests.
- **Expanded Declarative SQL Linter Rules & Profiles** (`internal/lint`, `docs/LINTER_SPEC.md`):
  - Published comprehensive declarative SQL review specification covering 80+ categorized rules across table design, foreign keys, index hygiene, concurrency, and security.
  - Implemented strict rules: `L014` (`PrimaryKeyTypeAllowlist`), `L022` (`DisallowReservedKeywords`), `L044` (`RequireBooleanDefault`), `L047` (`DisallowDuplicateIndex`), and `L048` (`DisallowRedundantPrefixIndex`).
  - Added rule profiles (`DefaultRules()`, `StrictRules()`) and CLI configuration filtering via `--strict`, `.grizzle.yaml` `disabled_rules`, and `fail_on` thresholds.

### Removed
- Pruned 292 lines of obsolete dead code: legacy single-column backfill helpers (`buildCopyBackfillSQL`, `lookupSingleColumnPK`), ad-hoc `cmd/grizzle/devurl.go`, uncalled `FormatGrantKey`, `GenerateAlterSubscriptionApplySQL`, `GenerateValidateFKSQL`, and deprecated `AcquireAdvisoryLock`.

## [v0.1.0] - 2026-10-13

### Added
- **Migration Exporter for golang-migrate**:
  - Export plan migrations to standard golang-migrate timestamped artifacts (`{timestamp}_migration.up.sql` and `{timestamp}_migration.down.sql`) via `--format migrate` (or `--format golang-migrate`).
  - Full reverse-down migration synthesis for reversible DDL changes, with explicit omission comments for irreversible mutations.
  - Public API support via `ExportFormatMigrate` and `ExportFormatGolangMigrate`.
- **Operational Polish & GA Freeze**:
  - Plan hash stability freeze: pinned golden hash test coverage across all plan step kinds.
  - Fast transaction-pooling / PgBouncer detection on session advisory lock acquisition.
  - SQLite rebuild crash recovery and actionable prior-failure error guidance.

## [v0.1.0-rc5] - 2026-10-13

### Fixed
- **PostgreSQL Shadow Compilation Target Schema Qualifiers** (`ec87caf`):
  - Fixed single-schema shadow compilation rewriting to rewrite target schema qualifiers (e.g. `public.foo`) to shadow-qualified references (e.g. `_grizzle_shadow.foo`) across functions, tables, views, triggers, and foreign keys.
  - Avoids collision with live catalog objects in the target schema during shadow compilation.

## [v0.1.0-rc4] - 2026-10-13

### Added
- **Operational Lifecycle & Onboarding Tooling**:
  - `grizzle status`: Detailed synchronization report and history summary across PostgreSQL and SQLite (in-sync verification, pending steps count, applied/partial/failed counts, latest run details; human-readable and `--json` format).
  - `grizzle history`: Query recorded migration history (`ListHistory`), with `--limit`, descending sort, and structured `--json` output.
  - `grizzle baseline`: Baseline pre-existing production databases without running DDL. Validates that the live database matches desired schema before baselining, with `--force` override.
  - `grizzle repair`: Resolve failed or interrupted migrations (`ResolveLatest`) by marking the record as `resolved` with an operator note, unblocking automated boot synchronization.
  - `grizzle dump`: Reverse-engineer live database catalogs into clean, canonical DDL SQL for PostgreSQL and SQLite. Excludes internal tables (`grizzle_history`, `sqlite_*`, `_grizzle_*`).
- **Developer Ergonomics & Configuration**:
  - `grizzle.yaml` and `grizzle.json`: Zero-dependency configuration file loader with automatic file discovery (`grizzle.yaml`, `.grizzle.yaml`, `grizzle.json`) and CLI flag fallback support.
  - Inline lint suppression directives: `-- grizzle:lint-ignore <rules>` for statement/table scope, and `-- grizzle:lint-disable <rules>` for global file-level suppression.
  - CLI pre/post migration hooks: `--before-sync` and `--after-sync` flags accepting SQL files or inline statements.
- **In-Process Boot Auto-Migration Engine Hardening** (`#52`):
  - Prior-failure fail-closed gating (`ErrPriorFailureBlocked`): `Sync()` inspects `grizzle_history` under advisory lock and aborts if the latest migration state is `partial` or `failed`. Added `Options.AllowResumePartial` and `WithAllowResumePartial()` for operator bypass.
  - Connection pool starvation safety: reuses held dedicated connection when recording failure history to prevent deadlocks on single-connection pools (`MaxOpenConns = 1`).
  - Explicit transaction isolation: enforces `sql.LevelReadCommitted` across all DDL transactions to protect against non-default database isolation levels.
  - Plan hash & tamper verification: added `Options.ExpectedHash` and `WithExpectedHash()` to verify plan hash integrity under lock before applying DDL.
  - Hardening test suite & benchmark allocs: added `b.ReportAllocs()` across all benchmarks, cold-boot migration benchmark, and hardening tests (`TestHardening_PriorFailureBlocking`, `TestHardening_SingleConnectionPool`, `TestHardening_ExpectedHashEnforcement`).

## [v0.1.0-rc3] - 2026-10-12

### Added
- **User-Defined Range & Composite Types** (`#37`, `#38`):
  - Declarative lifecycle for PostgreSQL custom range types (`CREATE TYPE ... AS RANGE`) and composite types (`CREATE TYPE ... AS (...)`).
  - In-place composite type attribute evolution (`ALTER TYPE ... ADD/DROP/ALTER ATTRIBUTE`).
  - Phased drop-and-recreate (`ChangeDropRangeRetype`, `ChangeDropCompositeRetype`) sorting at priority 28 before replacement type creation.
  - Live-only type drops gated behind `AllowDropRangeType`, `AllowDropCompositeType`, or `AllowDropType` + `DROP_RANGE_TYPE` / `DROP_COMPOSITE_TYPE` (CRITICAL) and sorting at priority 101 with `DROP TYPE IF EXISTS`.
- **Standalone and Owned Sequences** (`#29`):
  - Declarative sequence management for standalone `CREATE SEQUENCE` declarations in schema SQL.
  - Diffing and synchronization of sequence parameters (`INCREMENT BY`, `MINVALUE`, `MAXVALUE`, `CACHE`, `CYCLE`) and ownership (`OWNED BY table.col` vs `OWNED BY NONE`).
  - Automatic exclusion of implicit identity and serial sequences (`deptype = 'i'`).
  - Sequence drops sort after table drops (`priority 102`) with `DROP SEQUENCE IF EXISTS` to prevent cascade collisions.
- **In-Place Policy Evolution** (`#33`):
  - RLS policy drift in roles, `USING`, or `WITH CHECK` expressions now renders non-destructive `ALTER POLICY ... TO ... USING (...) WITH CHECK (...)` without requiring `AllowDropPolicy` or dropping the policy.
  - Full support for the PostgreSQL `PUBLIC` pseudo-role (`TO public`).
- **SQL Expression Normalization** (`#30`):
  - Quote-aware expression normalizer canonicalizes parentheses, strips redundant type casts (including parameterized types like `numeric(10,2)` and array casts), standardizes boolean operators (`IS TRUE` → `= true`), and strips shadow schema references.
  - Eliminates false-positive diffs on `CHECK` constraints, generated columns, RLS policy expressions, and partial index `WHERE` predicates.
- **Shadow Error Hints & Diagnostics** (`#31`):
  - Contextual remediation hints mapped from PostgreSQL error codes and messages during shadow transaction compilation (missing extensions, undefined functions, missing tables, misspelled columns, and duplicate identifiers).
- **Static Schema Lint Rules** (`#34`, `#32`, `#42`):
  - `L010` (`TriggerWithoutFunction`): Warns when triggers execute functions missing from desired schema routines.
  - `L011` (`UnversionedExtension`): Advises version pinning on `CREATE EXTENSION` statements.
  - `L012` (`NonIdempotentSeed`): Warns on non-idempotent statements in `SeedSQL` (bare `INSERT` without `ON CONFLICT` or `WHERE NOT EXISTS`, unconditional `DELETE`, and `TRUNCATE`). Exports `BuildIdempotentInsert` helper.
  - `L013` (`UnsupportedSQLiteFeature`): Detects PostgreSQL-only features in SQLite schemas (`COMMENT ON`, `ENABLE ROW LEVEL SECURITY`, `CREATE POLICY`).
- **Enum Value Renames** (`#39`):
  - Supports `ALTER TYPE ... RENAME VALUE 'old' TO 'new'` mapped via `Options.Renames`. Removed enum labels emit `DROP_ENUM_VALUE` (CRITICAL).
- **Supported Postgres Objects Matrix & Stability Docs** (`#35`, `#36`, `#41`):
  - Added structured Supported Objects Matrix to `README.md` categorizing managed vs protected-only constructs across 8 domains.
  - Stabilized and frozen API shapes for `Options.Renames`, `Options.ExpandContract`, and `Options.Backfill`.
  - Comprehensive golden plan hash coverage in `internal/plan/plan_hash_test.go` and integration tests in `expand_contract_test.go`.
  - Documented zero-downtime partition attach pattern using pre-validated `CHECK` constraints.
- **Benchmark Suite**:
  - Expanded `bench_test.go` covering warm-boot synchronization, no-drift diffing, fresh table planning, plan hashing, expression normalization, and Kahn topological DAG sorting.

### Fixed
- **Plan Hash Policy & Step Coverage**: `Plan.Hash()` now digests all 21 drop policy gates and step metadata (`IsSignatureChange`, `IsDependencyRebuild`, enum values, range/composite definitions, sequence parameters).
- **Drop Policy Inheritance**: `optionsFromPlan` preserves `AllowDropType`; `options.go` derives `AllowRangeType` and `AllowCompositeType` from `AllowType` without blanket `AllowDrop` override defeating explicit restrictions.
- **Enum Value Rename Qualification**: `GenerateRenameEnumValueSQL` schema-qualifies type identifiers via `quoteQualifiedIdentifier`, ensuring renames target the correct type in multi-schema environments.
- **Sequence Ownership & MinValue Cleanup**: `GenerateAlterSequenceSQL` correctly handles 3-part names, requires both table and column before emitting ownership clauses (falling back to `OWNED BY NONE`), and suppresses spurious `NO MINVALUE` when minvalue is default 1; sequence dependency inspection restricts joins to `deptype = 'a'`.
- **Function Signature Ordering** (`#40`): Replaced routines emit `DROP FUNCTION` before `CREATE FUNCTION` when identity argument types drift.
- **Identifier Quoting in Seed Builder**: `BuildIdempotentInsert` quotes conflict targets and update columns to prevent SQL injection and support SQL reserved keywords.
- **Routine Signature Normalization**: Canonicalizes routine argument types using `NormalizeIdentityArgs` before routine signature comparison.
- **Comment-Aware Lint Rules**: Lint rules for SQLite and extension version pinning strip comments prior to regex matching; seed lint rule supports CTE prefixes (`WITH ... INSERT`).

## [v0.1.0-rc2] - 2026-10-07

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
- **RolesSQL privilege sync** (`feat(plan): RolesSQL privilege sync`): new side-channel contract for grants/roles (PostgreSQL only; CLI `--roles roles.sql`, library `Options.RolesSQL`). Statement-scanned `CREATE ROLE`/`GRANT` statements are diffed against live `pg_authid` roles and object ACLs (`aclexplode`) and applied after all schema DDL. Managed roles are always `NOLOGIN` and stamped with a `grizzle-managed` catalog comment — only marker-stamped roles are dropped, and only behind `AllowDropRole` + `DROP_ROLE` (CRITICAL), with an ownership refusal when the role owns cluster objects. Missing grants become `GRANT`; surplus grants become `REVOKE` behind `AllowRevoke` + `REVOKE_PRIVILEGE` (CRITICAL); grant-option drift renders `WITH GRANT OPTION` / `REVOKE GRANT OPTION FOR`. Grants to `PUBLIC` or to roles Grizzle does not manage are never revoked; a desired `GRANT ... TO PUBLIC` emits `GRANT_PUBLIC` (WARNING). Unqualified `TABLE`/`SEQUENCE` objects resolve against the primary target schema. `RolesSQL` participates in `Plan.Hash()` when non-empty; SQLite + non-empty `RolesSQL` is rejected (`ErrInvalidOptions`). New `Options.AllowRevoke` / `Options.AllowDropRole` fields (inheriting from `AllowDrop`) round-trip through the plan artifact so policy stays approval-consistent across the post-lock re-diff. New example: `examples/roles-grants/roles.sql`.
- **CatalogSQL sync** (`feat(plan): CatalogSQL sync`): new side-channel contract for cluster-catalog objects (PostgreSQL only; CLI `--catalog catalog.sql`, library `Options.CatalogSQL`). Statement-scanned `CREATE PUBLICATION` / `CREATE EVENT TRIGGER` statements are diffed against live `pg_publication` / `pg_event_trigger` catalogs and applied after roles and grants. Publications diff on membership (`FOR TABLE`, `FOR ALL TABLES`, `FOR TABLES IN SCHEMA` — PostgreSQL 15+ for schema-level) and `publish` flags (absent clause defaults to all four DML operations); drift renders `ALTER PUBLICATION`. Event triggers diff on event, `WHEN TAG IN` filter, function, and enabled state — definition drift is DROP+CREATE (no in-place ALTER), enabled-only drift renders `ALTER EVENT TRIGGER ENABLE/DISABLE`. The sync aborts up front when an event-trigger function is missing. Drops are narrow: only objects stamped with the `grizzle-managed` catalog comment are considered, gated behind `AllowDropPublication` / `AllowDropEventTrigger` + `DROP_PUBLICATION` / `DROP_EVENT_TRIGGER` (CRITICAL). `EVENT_TRIGGER_SUPERUSER` (WARNING) fires on every event-trigger step; `PUBLICATION_ALL_TABLES` (NOTICE) on FOR ALL TABLES breadth. `CatalogSQL` participates in `Plan.Hash()` when non-empty; SQLite + non-empty `CatalogSQL` is rejected (`ErrInvalidOptions`). New example: `examples/catalog-sync/`.

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
  - Staged non-destructive migrations enabled via `Options.ExpandContract` (API shapes and hash inputs frozen).
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
