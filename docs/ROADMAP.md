# Grizzle Development Roadmap

This document outlines the step-by-step execution plan to build, test, and ship **Grizzle** into a finalized production-ready library.

---

## Phase 1: Core Engine & PostgreSQL MVP 🎯

**Objective**: Deliver a working in-process `Sync()` function for standard PostgreSQL tables and columns.

- [x] **1.1 Project Scaffolding**
  - Initialize `go.mod` (module `github.com/yourorg/grizzle`).
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
  - Add `pg_advisory_xact_lock` integration.
- [x] **1.6 Integration Testing**
  - Setup integration tests against live PostgreSQL using `testcontainers-go` or local docker-compose.
  - Verify clean boot with zero diffs takes `< 50ms`.

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
- [x] **5.4 Version 1.0.0 Ready**
  - All test suites passing with race detector, zero static analysis issues.
