# Grizzle specification

This document defines the technical specification, API contract, and safety model for Grizzle.

## 1. Scope and engine compatibility

* **Language**: Go 1.27+
* **Engines supported**:
  * PostgreSQL 13, 14, 15, 16, 17+
  * SQLite 3.35+ (via pure-Go `modernc.org/sqlite`, zero Cgo)
* **Database drivers supported**:
  * PostgreSQL: `github.com/jackc/pgx/v5/stdlib`, `github.com/lib/pq`
  * SQLite: `modernc.org/sqlite`, `github.com/mattn/go-sqlite3`
* **Dependencies**: Pure Go standard library and driver interfaces. Zero external CLI binaries or Docker containers.

## 2. API contract

### 2.1 Primary functions

```go
// Sync synchronizes the live database schema to match opts.SchemaSQL.
func Sync(ctx context.Context, db *sql.DB, opts Options) error

// PlanDiff computes the migration plan without executing any changes on the target database.
func PlanDiff(ctx context.Context, db *sql.DB, opts Options) (*Plan, error)
```

### 2.2 Configuration options

```go
type Options struct {
    // Dialect explicitly defines the database engine (DialectPostgres or DialectSQLite).
    // If empty, Grizzle detects the dialect automatically from the driver type.
    Dialect Dialect

    // SchemaSQL contains the complete DDL representing the desired state.
    // Typically embedded at build time with //go:embed schema.sql.
    SchemaSQL string

    // TargetSchema is the PostgreSQL schema to manage (defaults to "public").
    TargetSchema string

    // ShadowSchema is the temporary schema name used for validation (defaults to "_grizzle_shadow").
    ShadowSchema string

    // AllowDrop permits all destructive operations when set to true.
    // Defaults to false for zero data loss.
    AllowDrop bool

    // Granular drop overrides (nil inherits from AllowDrop):
    AllowDropTable  *bool
    AllowDropColumn *bool
    AllowDropIndex  *bool
    AllowDropFK     *bool

    // ExcludeTables defines table names or glob patterns (e.g. "spatial_ref_sys", "asynq_*")
    // that Grizzle will never alter, diff, or drop.
    ExcludeTables []string

    // IncludeTables restricts management scope to only the specified tables or patterns.
    // If empty, Grizzle manages all tables declared in SchemaSQL while respecting ExcludeTables.
    IncludeTables []string

    // LockID is a 64-bit integer for pg_advisory_xact_lock.
    // Defaults to a stable hash of TargetSchema.
    LockID int64

    // DryRun outputs planned statements without executing them.
    DryRun bool

    // Logger accepts a structured logger (*slog.Logger) for migration events.
    Logger *slog.Logger
}
```

### 2.3 Plan, hazard analysis, and visualization

```go
type Plan struct {
    TargetSchema string
    Steps        []Step
    Policy       DropPolicy
}

func (p *Plan) Additions() int
func (p *Plan) Modifications() int
func (p *Plan) Deletions() int
func (p *Plan) Blocked() int
func (p *Plan) Summary() (adds, alters, drops, blocked int)
func (p *Plan) Hazards() []Hazard
func (p *Plan) Format(w io.Writer, useColor bool) error
func (p *Plan) String() string

type HazardLevel string

const (
    HazardLevelCritical HazardLevel = "CRITICAL" // Data destruction (DROP TABLE, DROP COLUMN)
    HazardLevelWarning  HazardLevel = "WARNING"  // Execution risk (NOT NULL without DEFAULT)
    HazardLevelNotice   HazardLevel = "NOTICE"   // Locking or performance impact (INDEX creation/drop)
)

type Hazard struct {
    Level       HazardLevel `json:"level"`
    Type        ChangeType  `json:"type"`
    Table       string      `json:"table"`
    Description string      `json:"description"`
    SQL         string      `json:"sql"`
}
```

## 3. Supported schema constructs

### PostgreSQL

| Construct | Supported | Notes |
| :--- | :---: | :--- |
| `CREATE TABLE` | Yes | Composite primary keys, unlogged tables |
| `DROP TABLE` | Yes | Guarded by `AllowDropTable` |
| `ADD COLUMN` | Yes | Default expressions, nullability, generated columns |
| `DROP COLUMN` | Yes | Guarded by `AllowDropColumn` |
| `ALTER COLUMN TYPE` | Yes | Automatic `USING` cast generation |
| `ALTER COLUMN SET/DROP NOT NULL` | Yes | Fully supported |
| `ALTER COLUMN SET/DROP DEFAULT` | Yes | Literal, function, and interval defaults |
| `CREATE INDEX` | Yes | B-tree, GIN, GiST, BRIN, unique, partial (`WHERE`), expressions |
| `DROP INDEX` | Yes | Guarded by `AllowDropIndex` |
| `FOREIGN KEY` | Yes | `ON DELETE` / `ON UPDATE` actions (CASCADE, SET NULL, RESTRICT) |
| `ENUM Types` | Yes | `CREATE TYPE ... AS ENUM`, `ALTER TYPE ... ADD VALUE` |
| `Native Types` | Yes | UUID, JSONB, Arrays, Timestamps, Numerics |

### SQLite

| Construct | Supported | Notes |
| :--- | :---: | :--- |
| `CREATE TABLE` | Yes | Primary keys, autoincrement, column constraints |
| `DROP TABLE` | Yes | Guarded by `AllowDropTable` |
| `ADD COLUMN` | Yes | Direct `ALTER TABLE ... ADD COLUMN` when constraints permit |
| `DROP COLUMN` | Yes | Handled via SQLite 12-step table rebuild |
| `ALTER COLUMN TYPE` | Yes | Handled via SQLite 12-step table rebuild |
| `CREATE INDEX` | Yes | Direct `CREATE INDEX` and `CREATE UNIQUE INDEX` |
| `DROP INDEX` | Yes | Guarded by `AllowDropIndex` |
| `FOREIGN KEY` | Yes | Validated with `PRAGMA foreign_key_check` |

## 4. Safety model and invariants

### Invariant 1: Non-destructive by default
If `AllowDrop` is false (the default), Grizzle refuses to execute any plan containing table or column deletions. It returns a `*DestructiveViolationError` detailing the blocked operations.

### Invariant 2: Distributed mutual exclusion
PostgreSQL migrations acquire `pg_advisory_xact_lock(lock_id)` inside the active transaction. Concurrent pod boots queue behind the lock and resume only after the first transaction commits. Subsequent pods detect zero pending changes and start immediately.

### Invariant 3: Pre-flight shadow validation
`SchemaSQL` is validated in an isolated shadow schema (`_grizzle_shadow` or an in-memory SQLite database) before inspecting or touching the live schema. SQL errors stop execution without altering live tables.

### Invariant 4: Third-party table preservation
Tables matching `ExcludeTables` or known extension patterns (`spatial_ref_sys`, `geometry_columns`) are excluded from diffing, drop detection, and modification. Whitelisting with `IncludeTables` restricts Grizzle exclusively to named tables.

### Invariant 5: Transactional atomicity
All migration statements run in a single transaction. If any statement fails, the entire transaction rolls back.

## 5. Performance standards

* **Warm boot (0 pending diffs)**: `< 15ms` execution time.
* **Cold boot (initial schema creation)**: `< 80ms`.
* **Memory usage**: `< 10MB` heap allocations during diffing.
* **Database connections**: Uses 1 connection from the pool during migration.

## 6. Error taxonomy

```go
var (
    ErrEmptySchema         = errors.New("grizzle: schema SQL cannot be empty")
    ErrDestructiveBlocked  = errors.New("grizzle: destructive change rejected by policy")
    ErrLockAcquisition     = errors.New("grizzle: failed to acquire advisory lock")
    ErrCompilationFailed   = errors.New("grizzle: schema compilation in shadow schema failed")
    ErrExecutionFailed     = errors.New("grizzle: applying DDL to live schema failed")
    ErrInspectionFailed    = errors.New("grizzle: inspecting schema failed")
    ErrUnsupportedDialect  = errors.New("grizzle: unsupported database dialect")
)
```
