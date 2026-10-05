# Grizzle Specification (SPEC)

This document outlines the formal technical and functional specification for **Grizzle**.

---

## 1. Scope & Compatibility

* **Language**: Go 1.22+
* **Primary Target Engine**: PostgreSQL 13, 14, 15, 16, 17+
* **Go Drivers Supported**: Standard `database/sql` interfaces, compatible with `github.com/jackc/pgx/v5/stdlib` and `github.com/lib/pq`.
* **Zero External Dependencies**: Standard library only (no Cgo, no Atlas binaries, no CLI tools).

---

## 2. API Contract

### 2.1 Primary Function Signature

```go
func Sync(ctx context.Context, db *sql.DB, opts Options) error
```

### 2.2 Configuration Options

```go
type Options struct {
    // SchemaSQL contains the complete DDL representing the desired state.
    // Usually supplied via //go:embed schema.sql.
    SchemaSQL string

    // TargetSchema is the schema to manage (defaults to "public").
    TargetSchema string

    // ShadowSchema is the temporary schema name used for compilation (defaults to "_grizzle_shadow").
    ShadowSchema string

    // AllowDrop permits destructive changes (DROP TABLE, DROP COLUMN, DROP INDEX).
    // Defaults to false for safety.
    AllowDrop bool

    // LockID is a 64-bit integer used for pg_advisory_xact_lock.
    // Defaults to a stable hash of the TargetSchema name.
    LockID int64

    // DryRun returns planned SQL statements without executing them on the live database.
    DryRun bool

    // Logger accepts a custom logging hook for migration events.
    Logger Logger
}

type Plan struct {
    Steps []Step
}

type Step struct {
    Type        string
    Table       string
    SQL         string
    Destructive bool
}

// PlanDiff generates the planned migration steps without applying them.
func PlanDiff(ctx context.Context, db *sql.DB, opts Options) (*Plan, error)
```

---

## 3. Supported Schema Constructs

Grizzle supports the full breadth of standard PostgreSQL DDL by leveraging PostgreSQL's native parser:

| Construct | Supported | Notes |
| :--- | :---: | :--- |
| `CREATE TABLE` | ✅ | Full support with composite primary keys |
| `DROP TABLE` | ✅ | Guarded by `AllowDrop: true` |
| `ADD COLUMN` | ✅ | Supports default expressions and nullability |
| `DROP COLUMN` | ✅ | Guarded by `AllowDrop: true` |
| `ALTER COLUMN TYPE` | ✅ | Automatically casts compatible types |
| `ALTER COLUMN SET/DROP NOT NULL` | ✅ | Supported |
| `ALTER COLUMN SET/DROP DEFAULT` | ✅ | Supported |
| `CREATE INDEX` | ✅ | Unique, non-unique, multicolumn, and partial (`WHERE`) |
| `DROP INDEX` | ✅ | Guarded by `AllowDrop: true` |
| `FOREIGN KEY` | ✅ | Full support for `ON DELETE` / `ON UPDATE` actions |
| `ENUM Types` | ✅ | Custom `CREATE TYPE ... AS ENUM` |
| `UUID / JSONB / Arrays` | ✅ | Full native type support |

---

## 4. Safety Model & Invariants

### Invariant 1: Non-Destructive by Default
If `Options.AllowDrop == false`, Grizzle guarantees that **no existing data is destroyed**.
* If a table present in `public` is missing from `schema.sql`, execution halts with `ErrDestructiveBlocked`.
* If a column present in `public` is missing from `schema.sql`, execution halts with `ErrDestructiveBlocked`.

### Invariant 2: Atomic Execution (All-or-Nothing)
All DDL operations (or the decision to perform none) execute within a single PostgreSQL transaction.
* If any statement fails (e.g. invalid type cast on existing rows), the entire migration rolls back.
* The live database is never left in an unrecoverable or half-migrated state.

### Invariant 3: Single-Writer Mutual Exclusion
* Execution is bounded by `pg_advisory_xact_lock(lock_id)`.
* Multiple concurrent application instances queue sequentially.
* The lock is freed on transaction commit or rollback, even in ungraceful crashes.

---

## 5. Performance Standards

* **Execution Overhead**:
  * Clean boot (0 pending diffs): `< 40ms` total latency.
  * Typical sync (1–5 table adjustments): `< 150ms`.
* **Memory Footprint**: `< 15MB` heap allocation during schema diffing.
* **Connection Consumption**: Uses exactly 1 connection from the `*sql.DB` pool during synchronization.

---

## 6. Error Taxonomy

Grizzle provides explicit sentinel errors:

```go
var (
    ErrEmptySchema        = errors.New("grizzle: schema SQL cannot be empty")
    ErrDestructiveBlocked = errors.New("grizzle: destructive change rejected by policy")
    ErrLockAcquisition    = errors.New("grizzle: failed to acquire advisory lock")
    ErrCompilationFailed  = errors.New("grizzle: schema compilation in shadow schema failed")
    ErrExecutionFailed    = errors.New("grizzle: applying DDL to live schema failed")
)
```
