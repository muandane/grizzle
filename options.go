package grizzle

import (
	"cmp"
	"fmt"
	"log/slog"
	"strings"

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

	// AcceptHazards lists critical hazard codes that are explicitly approved to execute.
	// Critical hazards not present in this list will cause Apply to fail with ErrHazardBlocked.
	AcceptHazards []plan.HazardCode

	// ExpectedHash defines the approved plan approval hash to verify before execution.
	// If the recomputed plan hash post-lock differs, execution aborts with ErrPlanDrift.
	ExpectedHash string

	// LockID is a 64-bit integer used for the PostgreSQL advisory lock (pg_advisory_xact_lock).
	LockID int64

	// DryRun returns the planned SQL statements without executing them on the live database.
	DryRun bool

	// Logger accepts a structured logger (*slog.Logger) for migration events.
	Logger *slog.Logger
}

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

// WithExpectedHash sets the approved plan hash to verify before execution.
func WithExpectedHash(hash string) Option {
	return func(o *Options) {
		o.ExpectedHash = hash
	}
}

// Validate checks whether the options are consistent and valid.
func (o *Options) Validate() error {
	if strings.TrimSpace(o.SchemaSQL) == "" {
		return ErrEmptySchema
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

func toScopeFilters(opts Options) scope.Filters {
	return scope.Filters{
		Includes: opts.IncludeTables,
		Excludes: opts.ExcludeTables,
	}
}

func defaultPostgresLockID(targetSchema string) int64 {
	schema := cmp.Or(targetSchema, "public")
	return GenerateLockID("grizzle", schema)
}
