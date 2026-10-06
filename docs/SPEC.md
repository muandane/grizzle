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

// Apply applies an approved migration plan to the database.
// If ExpectedHash is provided and the plan recomputed post-lock differs, Apply aborts with ErrPlanDrift.
func Apply(ctx context.Context, db *sql.DB, p *Plan, opts ApplyOpts) error

// Check inspects the live database and returns ErrDrift if the schema differs from opts.SchemaSQL.
// It is strictly read-only and never modifies the database.
func Check(ctx context.Context, db *sql.DB, opts Options) error

// Export generates migration artifacts for the plan in the requested format (sql, goose, atlas).
func Export(p *Plan, format ExportFormat, version string) ([]Artifact, error)
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

    // TargetSchema is the schema to manage (defaults to "public" for Postgres, "main" for SQLite).
    // Deprecated: Use TargetSchemas for multi-schema support.
    TargetSchema string

    // TargetSchemas specifies the database schemas to manage (defaults to [TargetSchema] or ["public"] for Postgres).
    TargetSchemas []string

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
    IncludeTables []string

    // StrictScope requires IncludeTables to be non-empty when true, preventing accidental unmanaged operations.
    StrictScope bool

    // AcceptHazards specifies explicitly accepted critical hazards.
    // Unaccepted critical hazards block execution with ErrHazardBlocked.
    AcceptHazards []HazardCode

    // NonConcurrentIndexes forces PostgreSQL index creations to run transactionally without CONCURRENTLY.
    NonConcurrentIndexes bool

    // LockNamespace specifies application namespace string for PostgreSQL advisory locking (defaults to "grizzle").
    LockNamespace string

    // LockID is an optional explicit 64-bit integer used for the PostgreSQL advisory lock.
    // If 0, Grizzle derives 2-int per-schema advisory locks using (hash32(LockNamespace), hash32(schema)).
    LockID int64

    // LockTimeout sets the maximum duration to wait for acquiring the advisory lock (defaults to 5s).
    LockTimeout time.Duration

    // StatementTimeout sets the maximum duration for any individual DDL statement (defaults to 5m).
    StatementTimeout time.Duration

    // MaxRetries specifies maximum retry attempts when encountering lock_timeout (defaults to 3).
    MaxRetries int

    // RandFloat provides an optional random source func returning in [0.0, 1.0) for deterministic jitter in tests.
    RandFloat func() float64

    // Renames maps old column names to new column names (e.g. "users.old_col": "new_col")
    // to disambiguate renames instead of treating them as DROP + ADD.
    Renames map[string]string

    // ExpandContract enables staged expand-and-contract zero-downtime migrations.
    ExpandContract bool

    // Backfill hook is executed during staged expand migration outside the DDL lock window in batches.
    Backfill BackfillFunc

    // DryRun outputs planned statements without executing them.
    DryRun bool

    // SQLiteRebuildThreshold defines row count threshold above which SQLite table rebuilds chunk data copying by keyset.
    SQLiteRebuildThreshold int

    // SQLiteRebuildBatchSize defines chunk size when copying data in batches during SQLite table rebuilds.
    SQLiteRebuildBatchSize int

    // Logger accepts a structured logger (*slog.Logger) for migration events.
    Logger *slog.Logger
}

type ApplyOpts struct {
    ExpectedHash  string
    AcceptHazards []HazardCode
    Backfill      BackfillFunc
}

type ExportFormat string

const (
    FormatSQL   ExportFormat = "sql"
    FormatGoose ExportFormat = "goose"
    FormatAtlas ExportFormat = "atlas"
)

type Artifact struct {
    Filename string
    Content  string
}
```

### 2.3 Plan, hazard analysis, and visualization

```go
type Plan struct {
    TargetSchema   string
    TargetSchemas  []string
    Steps          []Step
    Policy         DropPolicy
    IncludeTables  []string
    ExcludeTables  []string
    Renames        map[string]string
    ExpandContract bool
    SchemaSQL      string
}

func (p *Plan) Hash() string
func (p *Plan) Hazards() []Hazard
func (p *Plan) Additions() int
func (p *Plan) Modifications() int
func (p *Plan) Deletions() int
func (p *Plan) Blocked() int
func (p *Plan) Summary() (adds, alters, drops, blocked int)
func (p *Plan) Format(w io.Writer, useColor bool) error
func (p *Plan) String() string

type HazardLevel string

const (
    HazardLevelCritical HazardLevel = "CRITICAL" // Data destruction (DROP TABLE, DROP COLUMN, TYPE_NARROW, RENAME_AMBIGUOUS, UNMANAGED_DEPENDENCY)
    HazardLevelWarning  HazardLevel = "WARNING"  // Execution risk (NOT NULL without DEFAULT, GENERATED_REWRITE, PARTITION_ATTACH_SCAN, PARTITION_PENDING_DETACH)
    HazardLevelNotice   HazardLevel = "NOTICE"   // Locking or performance impact (INDEX creation/drop, FK drop)
)

type HazardCode string

const (
    HazardDropTable              HazardCode = "DROP_TABLE"
    HazardDropColumn             HazardCode = "DROP_COLUMN"
    HazardTypeNarrow             HazardCode = "TYPE_NARROW"
    HazardNotNullNoDefault       HazardCode = "NOT_NULL_NO_DEFAULT"
    HazardIndexBuild             HazardCode = "INDEX_BUILD"
    HazardDropIndex              HazardCode = "DROP_INDEX"
    HazardDropFK                 HazardCode = "DROP_FK"
    HazardRenameAmbiguous        HazardCode = "RENAME_AMBIGUOUS"
    HazardUnmanagedDependency    HazardCode = "UNMANAGED_DEPENDENCY"
    HazardGeneratedRewrite       HazardCode = "GENERATED_REWRITE"
    HazardPartitionAttachScan    HazardCode = "PARTITION_ATTACH_SCAN"
    HazardPartitionPendingDetach HazardCode = "PARTITION_PENDING_DETACH"
)

type Hazard struct {
    Code        HazardCode  `json:"code"`
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
| `DROP TABLE` | Yes | Guarded by `AllowDropTable` and `HazardDropTable` |
| `ADD COLUMN` | Yes | Default expressions, nullability, generated columns |
| `DROP COLUMN` | Yes | Guarded by `AllowDropColumn` and `HazardDropColumn` |
| `RENAME COLUMN` | Yes | Atomic rename via `ALTER TABLE ... RENAME COLUMN` or staged expand |
| `ALTER COLUMN TYPE` | Yes | Automatic `USING` cast; guarded by `HazardTypeNarrow` if narrowed |
| `ALTER COLUMN SET/DROP NOT NULL` | Yes | Guarded by `HazardNotNullNoDefault` if non-null without default |
| `ALTER COLUMN SET/DROP DEFAULT` | Yes | Literal, function, and interval defaults |
| `CREATE INDEX` | Yes | Emitted as `CONCURRENTLY` by default; B-tree, GIN, GiST, BRIN, unique, partial |
| `DROP INDEX` | Yes | Guarded by `AllowDropIndex` and `HazardDropIndex` |
| `FOREIGN KEY` | Yes | Split into `ADD CONSTRAINT ... NOT VALID` and `VALIDATE CONSTRAINT` |
| `ENUM Types` | Yes | `CREATE TYPE ... AS ENUM`, `ALTER TYPE ... ADD VALUE` |
| `Generated Columns` | Yes | `GENERATED ALWAYS AS (...) STORED`; expression rewrite triggers `GENERATED_REWRITE` hazard |
| `Native Types` | Yes | UUID, JSONB, Arrays, Timestamps, Numerics |

### SQLite

| Construct | Supported | Notes |
| :--- | :---: | :--- |
| `CREATE TABLE` | Yes | Primary keys, autoincrement, column constraints |
| `DROP TABLE` | Yes | Guarded by `AllowDropTable` |
| `ADD COLUMN` | Yes | Direct `ALTER TABLE ... ADD COLUMN` when constraints permit |
| `RENAME COLUMN` | Yes | Direct `ALTER TABLE ... RENAME COLUMN` when mapped explicitly |
| `DROP COLUMN` | Yes | Handled via SQLite 12-step table rebuild |
| `ALTER COLUMN TYPE` | Yes | Handled via SQLite 12-step table rebuild |
| `CREATE INDEX` | Yes | Direct `CREATE INDEX` and `CREATE UNIQUE INDEX` |
| `DROP INDEX` | Yes | Guarded by `AllowDropIndex` |
| `FOREIGN KEY` | Yes | Validated with `PRAGMA foreign_key_check` |
| `Generated Columns` | Yes | `STORED` and `VIRTUAL` supported; rebuild preserves generated definitions |

### Unmanaged database objects (Detected, Protected, Not Managed)

| Construct | Supported | Policy |
| :--- | :---: | :--- |
| `Views` & `Materialized Views` | Protected | Introspected and dependency-graphed; destructive changes blocked via `UNMANAGED_DEPENDENCY` |
| `Triggers` | Protected | Preserved on managed tables; drop/alter operations on dependencies blocked |
| `Functions` & `Procedures` | Protected | Introspected; column/table dependencies protected from destructive alterations |
| `Sequences` (unowned) | Protected | Never dropped or managed |
| `Domains` | Protected | Introspected and protected |

> [!NOTE]
> **Roadmap Note**: Declarative view, trigger, and function migrations are intentionally out of scope for automigrations. They are detected and protected from collateral damage, but not altered or dropped. Declarative management of view DDL will be evaluated in future releases.

## 4. Safety model and invariants

### Invariant 1: Non-destructive by default
If `AllowDrop` is false (the default), Grizzle refuses to execute any plan containing table or column deletions. It returns a `*DestructiveViolationError` detailing the blocked operations.

### Invariant 2: Distributed mutual exclusion with post-lock re-diffing
PostgreSQL migrations acquire advisory locks (`pg_advisory_xact_lock` or session lock for concurrent indexes). Grizzle recomputes the diff post-lock to avoid TOCTOU races.

### Invariant 3: Hazard gating
Critical hazards (`DROP_TABLE`, `DROP_COLUMN`, `TYPE_NARROW`, `RENAME_AMBIGUOUS`, `UNMANAGED_DEPENDENCY`) fail execution unless accepted via `AcceptHazards`. Operational warnings (`GENERATED_REWRITE`) alert callers to full table rewrites.

### Invariant 4: Plan/Apply approval hash
`Plan.Hash()` provides a deterministic digest. `Apply` verifies the post-lock hash against `ExpectedHash`, aborting with `ErrPlanDrift` on mismatch.

### Invariant 5: Timeouts and retry
`LockTimeout` and `StatementTimeout` protect production availability. PostgreSQL `lock_timeout` conflicts are retried with exponential backoff and randomized jitter.

### Invariant 6: Strict scope protection
`StrictScope` ensures `IncludeTables` is provided, preventing accidental mutations in shared databases.

### Invariant 7: Expand and contract
Ambiguous column renames are blocked. Explicit mappings can execute as atomic renames or staged dual-column expansions for zero downtime.

### Invariant 8: History and drift detection
Applied plans are audited in `grizzle_history`. `Check()` provides read-only schema drift verification.

## 5. Error taxonomy

```go
var (
    ErrEmptySchema        = errors.New("grizzle: schema SQL cannot be empty")
    ErrDestructiveBlocked = errors.New("grizzle: destructive change rejected by policy")
    ErrLockAcquisition    = errors.New("grizzle: failed to acquire advisory lock")
    ErrCompilationFailed  = errors.New("grizzle: schema compilation in shadow schema failed")
    ErrExecutionFailed    = errors.New("grizzle: applying DDL to live schema failed")
    ErrInspectionFailed   = errors.New("grizzle: inspecting schema failed")
    ErrUnsupportedDialect = errors.New("grizzle: unsupported database dialect")
    ErrHazardBlocked      = errors.New("grizzle: migration blocked by unaccepted critical hazards")
    ErrPlanDrift          = errors.New("grizzle: plan drifted from approved state")
    ErrDrift              = errors.New("grizzle: live database schema has drifted from desired schema")
    ErrLockTimeout             = errors.New("grizzle: lock acquisition timed out")
    ErrInvalidOptions          = errors.New("grizzle: invalid options")
    ErrStrictScope             = errors.New("grizzle: strict scope requires non-empty IncludeTables")
    ErrPartitionConversion     = errors.New("grizzle: in-place conversion between regular and partitioned table is unsupported")
    ErrUnsupportedMultiSchema  = errors.New("grizzle: multi-schema configuration is unsupported on SQLite")
    ErrPartitionKeyNotInUnique = errors.New("grizzle: primary key or unique constraint must include all partition key columns")
)

type HazardError struct {
    Hazards []Hazard
}

type DriftError struct {
    Plan *Plan
}

type DestructiveViolationError struct {
    Violations []Step
}
```
