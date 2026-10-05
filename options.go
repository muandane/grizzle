package grizzle

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yourorg/grizzle/internal/plan"
	"github.com/yourorg/grizzle/internal/scope"
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

	// LockID is a 64-bit integer used for the PostgreSQL advisory lock (pg_advisory_xact_lock).
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

	// ExpandContract enables staged expand-and-contract zero-downtime migrations.
	// In expand mode, renamed or modified columns are added alongside existing columns,
	// delaying destructive drops to a later, separately approved plan.
	ExpandContract bool

	// Backfill hook function run outside the DDL lock window in batches during staged expand migration.
	Backfill BackfillFunc

	// DryRun returns the planned SQL statements without executing them on the live database.
	DryRun bool

	// Logger accepts a structured logger (*slog.Logger) for migration events.
	Logger *slog.Logger
}

// BackfillFunc defines the hook function signature for batch backfilling columns outside the DDL lock window.
type BackfillFunc func(ctx context.Context, tx *sql.Tx, table, oldCol, newCol string) error

// Option represents a functional option for configuring Options.
type Option func(*Options)

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

// WithDialect sets the database dialect.
func WithDialect(d Dialect) Option {
	return func(o *Options) {
		o.Dialect = d
	}
}

// WithTargetSchema sets the target schema.
func WithTargetSchema(schema string) Option {
	return func(o *Options) {
		o.TargetSchema = schema
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

	return plan.DropPolicy{
		AllowTable:  allowTable,
		AllowColumn: allowColumn,
		AllowIndex:  allowIndex,
		AllowFK:     allowFK,
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

func toScopeFilters(opts Options) scope.Filters {
	return scope.Filters{
		Includes:       opts.IncludeTables,
		Excludes:       opts.ExcludeTables,
		Strict:         opts.StrictScope,
		Renames:        opts.Renames,
		ExpandContract: opts.ExpandContract,
	}
}

func defaultPostgresLockID(targetSchema string) int64 {
	schema := cmp.Or(targetSchema, "public")
	return GenerateLockID("grizzle", schema)
}
