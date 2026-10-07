package grizzle

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
)

// Dialect specifies the target SQL database dialect.
type Dialect string

const (
	// DialectAuto instructs Grizzle to auto-detect the engine from the driver type.
	DialectAuto Dialect = ""
	// DialectPostgres specifies PostgreSQL (13+).
	DialectPostgres Dialect = "postgres"
	// DialectSQLite specifies SQLite (3.35+).
	DialectSQLite Dialect = "sqlite"
)

// Options configures the schema synchronization process.
type Options struct {
	// Dialect explicitly defines the database engine (DialectPostgres or DialectSQLite).
	Dialect Dialect

	// SchemaSQL contains the complete DDL representing the desired state.
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
	AllowDropCheck  *bool

	// ExcludeTables defines table names or glob patterns (e.g. "spatial_ref_sys", "asynq_*")
	// that Grizzle will never manage, alter, or drop.
	ExcludeTables []string

	// IncludeTables limits Grizzle's management scope to only the specified tables or patterns.
	IncludeTables []string

	// StrictScope requires IncludeTables to be non-empty. When true and IncludeTables is empty,
	// operations fail immediately with ErrStrictScope.
	// Strongly recommended for production environments to avoid accidental alterations or drops
	// of untracked tables.
	StrictScope bool

	// AcceptHazards lists critical hazard codes that are explicitly approved to execute.
	// Critical hazards not present in this list will cause Apply to fail with ErrHazardBlocked.
	AcceptHazards []plan.HazardCode

	// NonConcurrentIndexes opts out of emitting CONCURRENTLY for PostgreSQL index creation/drops.
	// When true, index operations are created inside the transaction.
	NonConcurrentIndexes bool

	// LockNamespace specifies the application namespace string used for PostgreSQL advisory locking (defaults to "grizzle").
	LockNamespace string

	// LockID is an optional explicit 64-bit integer used for the PostgreSQL advisory lock (pg_advisory_xact_lock).
	// If 0, Grizzle derives 2-int per-schema advisory locks using (hash32(LockNamespace), hash32(schema)).
	LockID int64

	// LockTimeout specifies the maximum time to wait when acquiring locks (defaults to 5s).
	LockTimeout time.Duration

	// StatementTimeout specifies the maximum execution time for any single DDL statement (defaults to 5m).
	StatementTimeout time.Duration

	// MaxRetries specifies the number of retry attempts upon lock timeout (SQLSTATE 55P03, defaults to 3).
	MaxRetries int

	// RandFloat provides an optional random source func returning in [0.0, 1.0) for deterministic jitter in tests.
	RandFloat func() float64

	// Renames maps old column names to new column names (e.g. "users.old_col": "new_col" or "old_col": "new_col")
	// to explicitly disambiguate column renames instead of treating them as DROP + ADD.
	Renames map[string]string

	// ExpandContract enables staged expand-and-contract zero-downtime migrations (ZDM).
	// In expand mode, renamed or modified columns are added alongside existing columns,
	// delaying destructive drops to a later, separately approved plan.
	ExpandContract bool

	// Backfill hook function run outside the DDL lock window in batches during staged expand migration.
	Backfill BackfillFunc

	// BeforeSync runs once before any migration steps or locks are executed.
	// If it returns an error, the migration aborts before any DDL runs and no
	// history record is written.
	BeforeSync SyncHook

	// AfterSync runs once after all migration steps and history recording succeed.
	// If it returns an error, the DDL has already committed; the error is
	// wrapped with ErrAfterSyncFailed.
	AfterSync SyncHook

	// BeforeStep executes immediately prior to executing each plan step.
	// If it returns an error, the pending step is not executed; in a
	// transactional group the transaction is rolled back and history records
	// the failure with error "before_step hook: ...".
	BeforeStep StepHook

	// AfterStep executes immediately following the successful execution of each
	// plan step. If it returns an error in a transactional group, the
	// transaction is rolled back (reverting the step); in a non-transactional
	// group the step has already committed and history records "partial".
	AfterStep StepHook

	// DryRun returns the planned SQL statements without executing them on the live database.
	DryRun bool

	// DryRunLockTimeout bounds how long live dry-run verification waits on
	// table locks before failing fast (defaults to 2s).
	DryRunLockTimeout time.Duration

	// ExecuteHooksInDryRun allows BeforeStep/AfterStep hooks to run during
	// live dry-run verification. Defaults to false to avoid accidental
	// external side effects (webhooks, message publishing, etc.).
	ExecuteHooksInDryRun bool

	// SeedSQL contains idempotent data-seed SQL executed after a successful
	// sync (Sync DDL → AfterSync → Seed). The seed runs in a single
	// transaction and is skipped when the same seed (by content hash) was
	// already applied, unless SeedForce is set.
	SeedSQL string

	// SeedForce re-runs the seed even when the same seed hash was already
	// applied.
	SeedForce bool

	// SQLiteRebuildThreshold defines the row count threshold above which SQLite table rebuilds
	// chunk data copying by keyset to prevent journal memory exhaustion.
	// Defaults to 100000. Set to 0 to disable batching.
	SQLiteRebuildThreshold int

	// SQLiteRebuildBatchSize defines the chunk size when copying data in batches during SQLite table rebuilds.
	// Defaults to 10000.
	SQLiteRebuildBatchSize int

	// Logger accepts a structured logger (*slog.Logger) for migration events.
	Logger *slog.Logger

	// Tracer specifies an optional tracer (OpenTelemetry or custom) for observing migrations.
	Tracer Tracer
}

// Tracer defines the interface for tracing Grizzle lifecycle events.
type Tracer = exec.Tracer

// Span represents an active trace span recorded by a Tracer.
type Span = exec.Span

// BackfillFunc defines the hook function signature for batch backfilling columns outside the DDL lock window.
type BackfillFunc func(ctx context.Context, tx *sql.Tx, table, oldCol, newCol string) error

// HookContext provides invocation context and database access for a step hook.
// DBTX is bound to the executor of the pending step: a *sql.Tx for
// transactional groups or a *sql.Conn for non-transactional steps.
type HookContext = exec.HookContext

// StepHook executes custom imperative code immediately before or after each
// plan step. Hooks must be idempotent: if a later step fails and the migration
// is retried or resumed after partial execution, hooks may be invoked again.
type StepHook = exec.StepHook

// SyncHook executes custom imperative code once before or after the entire
// synchronization. It receives a dedicated connection (not a transaction)
// because the execution may contain non-transactional statements.
type SyncHook = exec.SyncHook

// Option represents a functional option for configuring Options.
type Option func(*Options)

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

// WithTracer sets the tracer.
func WithTracer(t Tracer) Option {
	return func(o *Options) {
		o.Tracer = t
	}
}

// WithDialect sets the database dialect.
func WithDialect(d Dialect) Option {
	return func(o *Options) {
		o.Dialect = d
	}
}

// WithTargetSchema sets the target schema.
// Deprecated: Use WithTargetSchemas for multi-schema support.
func WithTargetSchema(schema string) Option {
	return func(o *Options) {
		o.TargetSchema = schema
		o.TargetSchemas = []string{schema}
	}
}

// WithTargetSchemas sets the target schemas to manage.
func WithTargetSchemas(schemas ...string) Option {
	return func(o *Options) {
		o.TargetSchemas = schemas
		if len(schemas) > 0 {
			o.TargetSchema = schemas[0]
		}
	}
}

// WithAllowDrop sets the general drop permission.
func WithAllowDrop(allow bool) Option {
	return func(o *Options) {
		o.AllowDrop = allow
	}
}

// WithExcludeTables sets excluded tables and glob patterns.
func WithExcludeTables(tables ...string) Option {
	return func(o *Options) {
		o.ExcludeTables = tables
	}
}

// WithIncludeTables sets included tables.
func WithIncludeTables(tables ...string) Option {
	return func(o *Options) {
		o.IncludeTables = tables
	}
}

// WithAcceptHazards configures explicitly accepted critical hazard codes.
func WithAcceptHazards(hazards ...plan.HazardCode) Option {
	return func(o *Options) {
		o.AcceptHazards = append(o.AcceptHazards, hazards...)
	}
}

// WithNonConcurrentIndexes controls whether PostgreSQL index creation should run inside the transaction.
func WithNonConcurrentIndexes(disabled bool) Option {
	return func(o *Options) {
		o.NonConcurrentIndexes = disabled
	}
}

// WithLockNamespace sets the application namespace string for PostgreSQL advisory locks.
func WithLockNamespace(ns string) Option {
	return func(o *Options) {
		o.LockNamespace = ns
	}
}

// WithLockTimeout sets the maximum duration to wait for acquiring locks.
func WithLockTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.LockTimeout = d
	}
}

// WithStatementTimeout sets the maximum duration for any single migration DDL statement.
func WithStatementTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.StatementTimeout = d
	}
}

// WithMaxRetries sets the maximum retry attempts upon lock timeout conflict (SQLSTATE 55P03).
func WithMaxRetries(n int) Option {
	return func(o *Options) {
		o.MaxRetries = n
	}
}

// WithRandFloat sets a custom random float function for deterministic backoff jitter in tests.
func WithRandFloat(fn func() float64) Option {
	return func(o *Options) {
		o.RandFloat = fn
	}
}

// WithStrictScope enables strict scoping mode requiring non-empty IncludeTables.
func WithStrictScope(strict bool) Option {
	return func(o *Options) {
		o.StrictScope = strict
	}
}

// WithSQLiteRebuildBatching sets the threshold and batch size for chunked keyset copying during SQLite rebuilds.
func WithSQLiteRebuildBatching(threshold, batchSize int) Option {
	return func(o *Options) {
		o.SQLiteRebuildThreshold = threshold
		o.SQLiteRebuildBatchSize = batchSize
	}
}

// Validate checks whether the options are consistent and valid.
func (o *Options) Validate() error {
	if strings.TrimSpace(o.SchemaSQL) == "" {
		return ErrEmptySchema
	}
	if o.StrictScope && len(o.IncludeTables) == 0 {
		return ErrStrictScope
	}
	switch o.Dialect {
	case DialectAuto, DialectPostgres, DialectSQLite:
		return nil
	default:
		return fmt.Errorf("grizzle: unsupported dialect %q", o.Dialect)
	}
}

// resolveDropPolicy extracts the effective fine-grained drop policy from Options.
func resolveDropPolicy(opts Options) plan.DropPolicy {
	allowTable := opts.AllowDrop
	if opts.AllowDropTable != nil {
		allowTable = *opts.AllowDropTable
	}

	allowColumn := opts.AllowDrop
	if opts.AllowDropColumn != nil {
		allowColumn = *opts.AllowDropColumn
	}

	allowIndex := opts.AllowDrop
	if opts.AllowDropIndex != nil {
		allowIndex = *opts.AllowDropIndex
	}

	allowFK := opts.AllowDrop
	if opts.AllowDropFK != nil {
		allowFK = *opts.AllowDropFK
	}

	allowCheck := opts.AllowDrop
	if opts.AllowDropCheck != nil {
		allowCheck = *opts.AllowDropCheck
	}

	return plan.DropPolicy{
		AllowTable:  allowTable,
		AllowColumn: allowColumn,
		AllowIndex:  allowIndex,
		AllowFK:     allowFK,
		AllowCheck:  allowCheck,
	}
}

// WithRenames sets the explicit column rename mapping.
func WithRenames(renames map[string]string) Option {
	return func(o *Options) {
		o.Renames = renames
	}
}

// WithExpandContract enables or disables staged expand-and-contract zero-downtime migrations.
func WithExpandContract(expand bool) Option {
	return func(o *Options) {
		o.ExpandContract = expand
	}
}

// WithBackfill configures the batch backfill hook function for staged expand migrations.
func WithBackfill(fn BackfillFunc) Option {
	return func(o *Options) {
		o.Backfill = fn
	}
}

// WithBeforeSync registers a hook that runs once before any migration steps or
// locks are executed.
func WithBeforeSync(fn SyncHook) Option {
	return func(o *Options) {
		o.BeforeSync = fn
	}
}

// WithAfterSync registers a hook that runs once after all migration steps and
// history recording succeed.
func WithAfterSync(fn SyncHook) Option {
	return func(o *Options) {
		o.AfterSync = fn
	}
}

// WithBeforeStep registers a hook that runs immediately prior to each plan step.
func WithBeforeStep(fn StepHook) Option {
	return func(o *Options) {
		o.BeforeStep = fn
	}
}

// WithAfterStep registers a hook that runs immediately after each successful
// plan step.
func WithAfterStep(fn StepHook) Option {
	return func(o *Options) {
		o.AfterStep = fn
	}
}

// WithDryRun enables dry-run mode: the planned SQL is validated against the
// live database without executing it.
func WithDryRun() Option {
	return func(o *Options) {
		o.DryRun = true
	}
}

// WithDryRunLockTimeout sets the lock wait bound for live dry-run verification.
func WithDryRunLockTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.DryRunLockTimeout = d
	}
}

// WithExecuteHooksInDryRun allows BeforeStep/AfterStep hooks to run during
// live dry-run verification.
func WithExecuteHooksInDryRun() Option {
	return func(o *Options) {
		o.ExecuteHooksInDryRun = true
	}
}

// WithSeedSQL attaches idempotent seed SQL executed after a successful sync.
func WithSeedSQL(seedSQL string) Option {
	return func(o *Options) {
		o.SeedSQL = seedSQL
	}
}

// WithSeedForce re-runs the seed even when the same seed hash was already
// applied.
func WithSeedForce(force bool) Option {
	return func(o *Options) {
		o.SeedForce = force
	}
}

func toScopeFilters(opts Options) scope.Filters {
	return scope.Filters{
		Includes:       opts.IncludeTables,
		Excludes:       opts.ExcludeTables,
		Strict:         opts.StrictScope,
		Renames:        opts.Renames,
		ExpandContract: opts.ExpandContract,
	}
}
