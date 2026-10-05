# Changelog

All notable changes to Grizzle will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Breaking Changes
- **Module Path**: Renamed module path from `github.com/yourorg/grizzle` to `github.com/muandane/grizzle`. Update your `go.mod` and import statements accordingly.
- **`ExpectedHash` in `Options`**: Removed `ExpectedHash` and `WithExpectedHash` from `grizzle.Options`. Plan approval hashes must now be passed explicitly via `grizzle.Apply(ctx, db, plan, grizzle.ApplyOpts{ExpectedHash: ...})`.
- **Hazard Gating Blocking**: Critical hazards (`DROP_TABLE`, `DROP_COLUMN`, `TYPE_NARROW`, `RENAME_AMBIGUOUS`, `NOT_NULL_NO_DEFAULT`) now cause `Apply` to fail with `ErrHazardBlocked` by default unless explicitly listed in `AcceptHazards`.
- **Package Restructuring**: Internal engine logic has been relocated to sub-packages under `internal/` (`schema`, `scope`, `diff`, `plan`, `dialect`, `exec`, `history`). Public types and constructors remain accessible via the root `grizzle` package facade.

### Added
- **Staged Expand-and-Contract Migrations**:
  - `Options.ExpandContract: true` generates staged non-destructive plans.
  - Plan 1 adds new columns as nullable alongside existing columns without dropping old columns.
  - Plan 2 (contract phase) is emitted separately with its own deterministic approval hash to drop old columns after backfill.
  - Added `BackfillFunc` hook (`Options.Backfill` / `ApplyOpts.Backfill`) executed in batches outside the DDL lock window.
- **PostgreSQL Concurrency & Lock Management**:
  - Multi-pod migration lock coordination using dedicated session-level advisory locks (`pg_advisory_lock` / `pg_try_advisory_lock`) held across both transactional and non-transactional DDL groups.
  - `VALIDATE CONSTRAINT` steps partitioned into separate transaction groups after `ADD ... NOT VALID` commits, releasing `ACCESS EXCLUSIVE` table locks before table scans run under `SHARE UPDATE EXCLUSIVE`.
  - Non-concurrent index option (`Options.NonConcurrentIndexes`) for transactional testing and ephemeral environments.
- **Automated Invalid Index Recovery**:
  - PostgreSQL schema inspection checks `pg_index.indisvalid`.
  - Stale or invalid indexes left by failed `CREATE INDEX CONCURRENTLY` are automatically detected and repaired via `DROP INDEX CONCURRENTLY` and recreation.
- **Partial and Failed History Tracking**:
  - `grizzle_history` records `status` (`applied`, `partial`, `failed`), `failed_step`, and `error`.
  - Non-transactional failures and partial rollbacks record failure state on a fresh connection with a detached context.
- **Retry Safety**:
  - Lock acquisition retries (SQLSTATE `55P03`) are halted immediately after partial progress to prevent re-applying committed steps.
- **SQLite Engine Parity**:
  - Full feature parity for column renames, hazard detection (`HazardTypeNarrow`, `HazardRenameAmbiguous`, `HazardNotNullNoDefault`), and `StrictScope` enforcement.
  - Native column renames via `ALTER TABLE ... RENAME COLUMN` without full table rebuilds where supported.
- **Integration Test Strictness**:
  - Integration tests run under `//go:build integration` fail loudly if PostgreSQL/Docker is unavailable, preventing silent skips in CI.
