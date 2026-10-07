# Grizzle specification

This document defines the technical specification, API contract, and safety model for Grizzle.

## 1. Scope and engine compatibility

* **Language**: Go 1.27+
* **Engines supported**:
  * PostgreSQL 14, 15, 16, 17, 18 (tested in CI; 13 claim removed — untested)
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
    // Must use the reserved "_grizzle_shadow" prefix, be a valid identifier of at most 63 bytes,
    // and must not name a target or included schema; violations fail with ErrInvalidOptions.
    ShadowSchema string

    // AllowDrop permits all destructive operations when set to true.
    // Defaults to false for zero data loss.
    AllowDrop bool

    // Granular drop overrides (nil inherits from AllowDrop):
    AllowDropTable  *bool
    AllowDropColumn *bool
    AllowDropIndex  *bool
    AllowDropFK     *bool
    AllowDropCheck  *bool

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

    // LockTimeout sets the maximum TOTAL duration to wait for acquiring the advisory
    // lock across all retry attempts, including backoff (defaults to 5s). The value is
    // never reset per attempt; it also feeds the per-statement DDL lock_timeout.
    LockTimeout time.Duration

    // StatementTimeout sets the maximum duration for any individual DDL statement (defaults to 5m).
    StatementTimeout time.Duration

    // MaxRetries specifies maximum retry attempts when encountering lock_timeout (defaults to 3).
    MaxRetries int

    // RandFloat provides an optional random source func returning in [0.0, 1.0) for deterministic jitter in tests.
    RandFloat func() float64

    // Renames maps old column names to new column names (e.g. "users.old_col": "new_col")
    // to disambiguate renames instead of treating them as DROP + ADD.
    // Experimental: mapping format may change before 1.0.
    Renames map[string]string

    // ExpandContract enables staged expand-and-contract zero-downtime migrations.
    // Experimental: staged plan shape may change before 1.0.
    ExpandContract bool

    // Backfill hook is executed during staged expand migration outside the DDL lock window in batches.
    // Experimental: library-only; no CLI equivalent.
    Backfill BackfillFunc

    // BeforeSync runs once before any migration steps or locks execute; a
    // non-nil error aborts the migration before any DDL runs.
    BeforeSync SyncHook

    // AfterSync runs once after all migration steps and history recording succeed.
    AfterSync SyncHook

    // BeforeStep executes immediately prior to each plan step.
    BeforeStep StepHook

    // AfterStep executes immediately after each successful plan step.
    AfterStep StepHook

    // DryRun outputs planned statements without executing them.
    DryRun bool

    // DryRunLockTimeout bounds lock waits for live dry-run verification.
    DryRunLockTimeout time.Duration

    // SeedSQL contains idempotent data-seed SQL executed after a successful
    // sync (DDL -> AfterSync -> Seed); already-applied seeds are skipped
    // unless SeedForce is set.
    SeedSQL string

    // SeedForce re-runs the seed even when the same seed hash was already applied.
    SeedForce bool

    // SQLiteRebuildThreshold defines row count threshold above which SQLite table rebuilds chunk data copying by keyset.
    SQLiteRebuildThreshold int

    // SQLiteRebuildBatchSize defines chunk size when copying data in batches during SQLite table rebuilds.
    SQLiteRebuildBatchSize int

    // Logger accepts a structured logger (*slog.Logger) for migration events.
    Logger *slog.Logger

    // Tracer receives lifecycle spans (sync start/end, step execution, lock wait).
    Tracer Tracer
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

    // Approval-sensitive: participates in Hash() because it changes generated SQL.
    NonConcurrentIndexes bool

    // Operational timeouts persisted for direct Apply. Bounded by
    // ValidateExecutionFields; excluded from Hash(). Lock identity and
    // shadow schema names are never persisted — LockID is derived at apply
    // from trusted target identity, LockNamespace is an ApplyOpts runtime
    // option, and shadow schemas are generated ephemerally per run.
    LockTimeout      time.Duration
    StatementTimeout time.Duration
}

func (p *Plan) Hash() string // schema(s), scope, renames, expand, policy, NonConcurrentIndexes, SchemaSQL, ordered steps
func (p *Plan) ValidateExecutionFields() error
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
    HazardLevelCritical HazardLevel = "CRITICAL" // Data destruction (DROP TABLE/COLUMN, TYPE_NARROW, RENAME_AMBIGUOUS, UNMANAGED_DEPENDENCY, GENERATED_REWRITE)
    HazardLevelWarning  HazardLevel = "WARNING"  // Execution risk (PARTITION_ATTACH_SCAN, PARTITION_PENDING_DETACH)
    HazardLevelNotice   HazardLevel = "NOTICE"   // Locking or performance impact (INDEX creation/drop, FK drop)
)

// ValidateExecutionFields bounds persisted timeouts to [0, 24h].
// Lock identity and shadow schema names are not carried in the artifact.

type HazardCode string

const (
    HazardDropTable              HazardCode = "DROP_TABLE"
    HazardDropColumn             HazardCode = "DROP_COLUMN"
    HazardTypeNarrow             HazardCode = "TYPE_NARROW"
    HazardNotNullNoDefault       HazardCode = "NOT_NULL_NO_DEFAULT"
    HazardIndexBuild             HazardCode = "INDEX_BUILD"
    HazardDropIndex              HazardCode = "DROP_INDEX"
    HazardDropFK                 HazardCode = "DROP_FK"
    HazardDropCheck              HazardCode = "DROP_CHECK"
    HazardCheckValidateScan      HazardCode = "CHECK_VALIDATE_SCAN"
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
| `CHECK Constraint` | Yes | Named and inline `CHECK`; staged `ADD ... NOT VALID` then `VALIDATE CONSTRAINT`; drops guarded by `AllowDropCheck` |
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
| `CHECK Constraint` | No | SQLite CHECK constraints are not introspected or diffed; a table rebuild rewrites the table from the managed IR and does not preserve inline CHECK DDL — declare CHECK-managed tables as Postgres-only or avoid rebuild-triggering changes |
| `Generated Columns` | Yes | `STORED` and `VIRTUAL` supported; rebuild preserves generated definitions |

### Unmanaged database objects (Detected, Protected, Not Managed)

| Construct | Supported | Policy |
| :--- | :---: | :--- |
| `Views` & `Materialized Views` | Managed | Introspected (`pg_get_viewdef`); create / replace / drop synced declaratively, drops gated by `AllowDropView` |
| `Triggers` | Protected | Preserved on managed tables; drop/alter operations on dependencies blocked |
| `Functions` & `Procedures` | Protected | Introspected with table/column dependencies: `pg_depend` for SQL-standard (`BEGIN ATOMIC`) bodies, conservative source scan for quoted string bodies; destructive alterations on dependents blocked via `UNMANAGED_DEPENDENCY` |
| `Sequences` (unowned) | Protected | Never dropped or managed |
| `Domains` | Protected | Introspected and protected |

> [!NOTE]
> **Roadmap Note**: Declarative views, triggers, functions, RLS policies, and extensions are managed as of this release (see §3). Views and materialized views are created, replaced, and dropped per the desired schema; destructive view steps are gated behind `AllowDropView` and the `DROP_VIEW` hazard.

## 4. Safety model and invariants

### Invariant 1: Non-destructive by default
If `AllowDrop` is false (the default), Grizzle refuses to execute any plan containing table or column deletions. It returns a `*DestructiveViolationError` detailing the blocked operations.

### Invariant 2: Distributed mutual exclusion with post-lock re-diffing
PostgreSQL migrations acquire advisory locks (`pg_advisory_xact_lock` or session lock for concurrent indexes). Grizzle recomputes the diff post-lock to avoid TOCTOU races.

### Invariant 3: Hazard gating
Critical hazards (`DROP_TABLE`, `DROP_COLUMN`, `TYPE_NARROW`, `RENAME_AMBIGUOUS`, `UNMANAGED_DEPENDENCY`, `GENERATED_REWRITE`) fail execution unless accepted via `AcceptHazards`.

### Invariant 4: Plan/Apply approval hash
`Plan.Hash()` digests approval-sensitive intent (target schemas, scope, renames, expand/contract, policy, NonConcurrentIndexes, SchemaSQL, ordered steps). `Apply` / `DryRunVerifyPlan` verify against `ExpectedHash`, aborting with `ErrPlanDrift` on mismatch. History is advisory (Model B): DDL commits first; a failed history write returns `ErrHistoryRecord` while leaving schema changes applied.

### Invariant 5: Timeouts and retry
`LockTimeout` and `StatementTimeout` protect production availability. `LockTimeout` bounds the **total** advisory-lock acquisition wait across all retry attempts (including backoff); preamble work (hooks, schema setup, session timeout statements) and DDL execution do not consume the budget — each attempt's acquisition timer is armed when acquisition begins. PostgreSQL lock-contention and deadlock failures (SQLSTATE `55P03`, `40P01`) are classified via `exec.IsRetryable` and retried with exponential backoff and randomized jitter; retries halt immediately once any DDL step commits.

### Invariant 6: Strict scope protection
`StrictScope` ensures `IncludeTables` is provided, preventing accidental mutations in shared databases.

### Invariant 7: Expand and contract
Ambiguous column renames are blocked with `RENAME_AMBIGUOUS`; map them explicitly via `Options.Renames` (CLI: repeatable `--rename old=new`, optionally table-qualified `table.old=new`). Explicit renames execute either as atomic renames or, with `Options.ExpandContract` enabled (CLI: `--expand-contract`), as staged zero-downtime migrations:

1. **Expand plan**: renamed/modified columns are added alongside existing columns as nullable; nothing is dropped or rewritten.
2. **Backfill**: values are copied in batches outside the DDL lock window via the library-only `Options.Backfill` hook. There is no CLI backfill runner.
3. **Contract plan**: a second, separately-approved plan (own deterministic hash) drops the legacy columns once the application no longer reads them; destructive, so it requires `AllowDropColumn` and explicit `AcceptHazards`.

The `RENAME_AMBIGUOUS` hazard description and the interactive summary remediation point to `--rename`. See `examples/expand-contract` for a runnable end-to-end flow.

### Invariant 8: History and drift detection
Applied plans are audited in `grizzle_history`. `Check()` provides read-only schema drift verification. A failed success-path history write does not fail the applied migration: it is logged at ERROR and surfaced as a typed non-fatal error (`errors.Is(err, grizzle.ErrHistoryRecord)`); failure-path history write failures are logged and never swallowed.

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
    ErrHistoryRecord           = errors.New("grizzle: migration succeeded but history record was not written") // non-fatal
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

## 6. Schema linting

`LintSchema` statically checks the desired schema IR. The lint layer is pure: it imports only `internal/schema`, performs no I/O, and produces identical diagnostics for identical input DDL. `DefaultLintRules()` returns the built-in rule set:

| Rule | Severity | Check |
| :--- | :--- | :--- |
| `L001` | `ERROR` | Table has no primary key (partitions are skipped; the parent's key covers them) |
| `L002` | `WARNING` | Foreign key columns are not a prefix of any index (including the primary key) |
| `L003` | `WARNING` | Table, column, index, or enum name is not lowercase snake_case |
| `L004` | `WARNING` | Column uses a legacy `SERIAL` / `nextval` default instead of `GENERATED ALWAYS AS IDENTITY` |
| `L005` | `WARNING` | CHECK constraint name is not lowercase snake_case |
| `L006` | `WARNING` | Table declares multiple CHECK constraints with the same normalized expression |
| `L007` | `INFO` | CHECK constraint uses PostgreSQL's auto-generated name (`table[_column]_check[n]`); prefer an explicit `CONSTRAINT name CHECK` |

Severity semantics:

- `ERROR`: structural anti-pattern; `LintHasErrors` reports it and release gates should fail.
- `WARNING`: recommendation that may be ignored deliberately.
- `INFO`: stylistic suggestion with no correctness impact.

Custom rules implement the `LintRule` interface (`ID`, `Description`, `Check(*SchemaIR) []LintDiagnostic`) and may be mixed with `DefaultLintRules()`. Formatters: `LintFormatText` (human-readable with severity counts), `LintFormatJSON` (stable JSON array), `LintFormatGitHub` (workflow commands: `ERROR` → `::error`, `WARNING` → `::warning`, `INFO` → `::notice`).

```go
type LintDiagnostic struct {
    RuleID   string       `json:"rule_id"`
    Severity LintSeverity `json:"severity"`
    Table    string       `json:"table"`
    Column   string       `json:"column,omitempty"`
    Message  string       `json:"message"`
    Line     int          `json:"line,omitempty"`
}

type LintRule interface {
    ID() string
    Description() string
    Check(s *SchemaIR) []LintDiagnostic
}

func LintSchema(s *SchemaIR, rules ...LintRule) []LintDiagnostic
func DefaultLintRules() []LintRule
func LintHasErrors(diags []LintDiagnostic) bool
func LintFormatText(w io.Writer, diags []LintDiagnostic) error
func LintFormatJSON(w io.Writer, diags []LintDiagnostic) error
func LintFormatGitHub(w io.Writer, diags []LintDiagnostic) error
```
