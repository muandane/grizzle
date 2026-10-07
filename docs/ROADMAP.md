# Grizzle Development Roadmap

This document outlines the step-by-step execution plan to build, test, and ship **Grizzle** into a finalized production-ready library.

---

## Phase 1: Core Engine & PostgreSQL MVP 🎯

**Objective**: Deliver a working in-process `Sync()` function for standard PostgreSQL tables and columns.

- [x] **1.1 Project Scaffolding**
  - Initialize `go.mod` (module `github.com/muandane/grizzle`).
  - Configure linting and CI templates (golangci-lint, GitHub Actions).
- [x] **1.2 PostgreSQL Catalog Inspector**
  - Implement `inspectSchema()` using direct `pg_catalog` queries.
  - Implement type normalization matrix (handling `int4`/`serial`, `varchar`, `timestamptz`).
- [x] **1.3 Shadow Schema Runner**
  - Implement transaction-scoped `_grizzle_shadow` creation and isolation via `search_path`.
  - Implement guaranteed cleanup (`DROP SCHEMA ... CASCADE`) with Go `defer`.
- [x] **1.4 Diffing & DDL Generator**
  - Build state comparison for tables (`CREATE TABLE`) and columns (`ADD`, `DROP`, `ALTER`).
  - Implement basic topological sorting to ensure tables are created before columns are referenced.
- [x] **1.5 Advisory Locking**
  - Implement deterministic FNV-1a hash lock generation.
  - Session-level advisory locking (`pg_advisory_lock` / `pg_try_advisory_lock`) held across the full sync; superseded the original transaction-scoped `pg_advisory_xact_lock` design so locks survive non-transactional groups (e.g. `CREATE INDEX CONCURRENTLY`).
- [x] **1.6 Integration Testing**
  - Setup integration tests against live PostgreSQL using `testcontainers-go` or local docker-compose.
  - Verify clean boot with zero diffs stays within the 100ms CI test budget.

---

## Phase 2: Relational Depth (Indexes, FKs & Custom Types) 🔗

**Objective**: Support complex relational schemas including foreign key constraints, indexes, and custom enums.

- [x] **2.1 Index Management**
  - Extract indexes via `pg_get_indexdef`.
  - Detect added, dropped, or modified indexes (unique, multi-column, partial `WHERE` indexes).
- [x] **2.2 Foreign Key Constraints**
  - Extract foreign keys via `pg_get_constraintdef`.
  - Support `ON DELETE CASCADE / SET NULL / RESTRICT`.
  - Implement deferred foreign key creation (create tables first, link foreign keys afterward).
- [x] **2.3 Custom Enum Types**
  - Inspect `pg_type` for custom enum definitions.
  - Generate `CREATE TYPE ... AS ENUM` before table creation.
  - Support adding new enum values via `ALTER TYPE ... ADD VALUE`.
- [x] **2.4 Composite Primary Keys**
  - Support tables with multi-column primary keys.

---

## Phase 3: Developer Experience, Safety & Observability 🛡️

**Objective**: Give developers full visibility and rock-solid safety against accidental data loss.

- [x] **3.1 Plan & Dry-Run API**
  - Implement `grizzle.PlanDiff(ctx, db, opts) (*Plan, error)`.
  - Allow inspecting what SQL would be executed without running it.
- [x] **3.2 Terminal Diff Visualizer**
  - Provide a human-readable visual summary of planned changes:
    ```text
    + CREATE TABLE "organizations" (id, name, created_at)
    ~ ALTER TABLE "users" ADD COLUMN "org_id" bigint
    + CREATE INDEX "idx_users_org_id" ON "users" ("org_id")
    - DROP COLUMN "legacy_role" (BLOCKED by AllowDrop: false)
    ```
- [x] **3.3 Strict Safety Guards**
  - Implement fine-grained drop policies (`AllowDropTable: false`, `AllowDropColumn: false`, `AllowDropIndex: true`).
  - Provide actionable error messages explaining exactly which line in `schema.sql` triggered a destructive warning.
- [x] **3.4 Structured Logging**
  - Implement `Logger` interface to integrate with `log/slog` or custom application loggers.

---

## Phase 4: Embedded Database Support (SQLite) 🪶

**Objective**: Provide the exact same declarative experience for local testing and SQLite embedded apps.

- [x] **4.1 SQLite Shadow Runner**
  - Use in-memory SQLite isolation: isolated `sql.Open("sqlite", ":memory:")`.
- [x] **4.2 SQLite Catalog Inspector**
  - Query `sqlite_schema` and `PRAGMA table_info()` / `PRAGMA foreign_key_list()`.
- [x] **4.3 SQLite 12-Step Table Rebuild Engine**
  - Because SQLite does not support `ALTER COLUMN DROP/MODIFY`, implement the SQLite standard 12-step table recreation pattern:
    1. Create temp table `new_t`
    2. Copy data `INSERT INTO new_t SELECT ... FROM t`
    3. Drop old table `t`
    4. Rename `new_t` to `t`

---

## Phase 5: Production Hardening & Release 🚀

**Objective**: Battle-test under high concurrency and achieve v1.0.0 stability.

- [x] **5.1 Multi-Pod Race Condition Fuzzing**
  - Launch 50 concurrent goroutines against a single PostgreSQL instance attempting to run `Sync()` simultaneously.
  - Assert that all 50 succeed with zero deadlocks and exactly one migration execution.
- [x] **5.2 Performance Profiling**
  - Optimize catalog queries and memory allocations.
  - Target `< 30ms` latency (achieved ~5ms for PostgreSQL, ~0.15ms for SQLite).
- [x] **5.3 Documentation & Examples**
  - Write sample projects:
    - Pure Go + standard `database/sql` (`examples/postgres-stdlib`)
    - Embedded pure-Go SQLite (`examples/sqlite-embedded`)
    - Go + `sqlc` (`examples/sqlc-workflow`)
    - Zero-downtime expand/contract rename (`examples/expand-contract`)
- [x] **5.4 First release (v0.1.0)**
  - All test suites passing with race detector, zero static analysis issues.
  - Tag `v0.1.0-rc.1` from a green `main`; docs and code claims reconciled (see CONTRIBUTING release steps).
  - Not yet v1.0.0: the API carries `Experimental:` surfaces (renames, expand/contract, backfill) that may still change.

## Post-0.1 backlog

Ordered by expected impact; nothing here blocks the first release.

- [x] **Declarative management surface expansion** — shipped (see "Managed surface expansion" below): extensions, RLS + policies, functions, triggers, and views/matviews are now first-class managed constructs.
- [x] **SQLite declarative CHECK constraints** — named and inline CHECK constraints are parsed from `sqlite_schema.sql`, emitted on create/rebuild, and diffed (check drift triggers a rebuild; removals gated by `AllowDropCheck` + `DROP_CHECK`).
- [x] **Domain CHECK management** — domains are managed end-to-end: base type/nullability/default diffed from `pg_type` + `pg_constraint` (`conrelid = 0`), CHECK drift via `ALTER DOMAIN ADD/DROP CONSTRAINT`, drops gated by `AllowDropDomain` + `DROP_DOMAIN`.
- [ ] **CLI backfill runner** — batched backfill is library-only (`Options.Backfill`); a CLI runner would need a durable batching contract.
- [ ] **Multi-schema SQLite** — rejected today (`ErrUnsupportedMultiSchema`).
- [ ] **v1.0.0 API freeze** — remove `Experimental:` markers once rename mapping, staged plans, and backfill batching stabilize.

## Managed surface expansion (shipped)

Declarative lifecycle for objects previously detected-and-protected. Each construct follows the same pipeline: shadow-compile the desired DDL, introspect both live and shadow catalogs, diff canonically (`pg_get_*def` outputs), render steps, gate destructive steps.

- **Extensions** — statement-scanned (they roll back with the shadow tx, so they are captured before shadow compilation and re-installed best-effort for type availability). `EXTENSION_PRIVILEGE` warning; `AllowDropExtension` gate; intentionally irreversible in migration exports.
- **RLS + policies** — table flags via `pg_class`, policies via `pg_policy` with expression normalization. Replace is DROP+CREATE (PostgreSQL has no `CREATE OR REPLACE POLICY`). `RLS_ENABLE` warning, `DROP_POLICY` critical + `AllowDropPolicy`. Lint L008 flags RLS-enabled tables with zero policies.
- **COMMENT ON** — table/column comments diffed from `obj_description` / `col_description`; set/clear is non-destructive with a `COMMENT_CLEAR` notice when overwriting an existing comment; reversible in exports.
- **Functions** — canonical `pg_get_functiondef` comparison with search-path-independent inspection; body drift replaces in place, signature drift is DROP+CREATE. `DROP_FUNCTION` critical + `AllowDropFunction`; `SECURITY_DEFINER` warning.
- **Procedures** — managed like functions (`pg_get_functiondef` covers both); body drift → `CREATE OR REPLACE PROCEDURE`, signature drift DROP+CREATE, drops share the `AllowDropFunction` gate. Window functions (prokind `w`) remain protected.
- **Aggregates** — canonical definition reconstructed from `pg_aggregate` catalog fields (`pg_get_functiondef` does not support aggregates); any drift is DROP+CREATE (Postgres has no `CREATE OR REPLACE AGGREGATE`); created after and dropped before their support functions via dedicated `CREATE_AGGREGATE`/`DROP_AGGREGATE` plan steps sharing the function gate. Ordered-set/hypothetical aggregates remain protected.
- **Domains** — managed types diffed from `pg_type` + `pg_constraint` (`conrelid = 0`); base type/nullability/default drift is DROP+CREATE (retype drop sorts before the replacement create via `DROP_DOMAIN_RETYPE`), CHECK drift is `ALTER DOMAIN ADD/DROP CONSTRAINT`; drops gated by `AllowDropDomain` + `DROP_DOMAIN` (CRITICAL), no implicit `CASCADE`. Extension-owned domains (`pg_depend.deptype = 'e'`) remain excluded.
- **Triggers** — `pg_get_triggerdef` canonical; surviving managed triggers fold into `UNMANAGED_DEPENDENCY` so dependent column drops stay blocked until the hazard is accepted. `DROP_TRIGGER` critical + `AllowDropTrigger`.
- **Views / materialized views** — `pg_get_viewdef` canonical; append-only column growth replaces in place, everything else is DROP+CREATE; matviews additionally emit `REFRESH MATERIALIZED VIEW`. `DROP_VIEW` critical + `AllowDropView`.
- **Lint L009** — rejects DML in `SchemaSQL` (silently ignored today; seeds belong in `SeedSQL`).
- **Export** — reversal (`Down`) SQL for policy/RLS/function/trigger/view/comment creation (comments restore the previous text; aggregates reverse to `DROP AGGREGATE`; domain creation and `ALTER DOMAIN ... ADD CONSTRAINT` reverse to `DROP DOMAIN` / `DROP CONSTRAINT`; `GRANT`/`REVOKE` reverse to each other, `CREATE ROLE` reverses to `DROP ROLE`); extension creation deliberately irreversible.
- **Roles & grants (`RolesSQL`)** — shipped as a side-channel contract (see SPEC §2.2 "RolesSQL contract"): `--roles roles.sql` statement-scans `CREATE ROLE`/`GRANT`, diffs against live `pg_authid` + ACLs, applies after all schema DDL. Managed `NOLOGIN` roles are marker-stamped; drops gated by `AllowDropRole` + `DROP_ROLE` (CRITICAL) with ownership refusal; revocations gated by `AllowRevoke` + `REVOKE_PRIVILEGE` (CRITICAL); `GRANT_PUBLIC` warning; grants to `PUBLIC`/unmanaged grantees are never revoked. Passwords and `ALTER ROLE` config stay out of scope.

### Deferred (rationale)

- **Publications / event triggers** — cluster-wide, schema-less objects with no shadow-compile story (the shadow schema is transactional; publications and event triggers are database-level).
