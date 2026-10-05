package grizzle

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/yourorg/grizzle/internal/dialect/postgres"
	"github.com/yourorg/grizzle/internal/dialect/sqlite"
	"github.com/yourorg/grizzle/internal/diff"
	"github.com/yourorg/grizzle/internal/exec"
	"github.com/yourorg/grizzle/internal/plan"
	"github.com/yourorg/grizzle/internal/schema"
	"github.com/yourorg/grizzle/internal/scope"
)

// Public type aliases
type (
	Plan        = plan.Plan
	Step        = plan.Step
	ChangeType  = plan.ChangeType
	DropPolicy  = plan.DropPolicy
	HazardLevel = plan.HazardLevel
	HazardCode  = plan.HazardCode
	Hazard      = plan.Hazard

	SchemaIR     = schema.Schema
	TableIR      = schema.Table
	ColumnIR     = schema.Column
	IndexIR      = schema.Index
	ForeignKeyIR = schema.ForeignKey
	PrimaryKeyIR = schema.PrimaryKey
	EnumIR       = schema.Enum
)

// ChangeType constants
const (
	ChangeCreateEnum  = plan.ChangeCreateEnum
	ChangeAlterEnum   = plan.ChangeAlterEnum
	ChangeCreateTable = plan.ChangeCreateTable
	ChangeDropTable   = plan.ChangeDropTable
	ChangeAddColumn   = plan.ChangeAddColumn
	ChangeDropColumn  = plan.ChangeDropColumn
	ChangeAlterColumn = plan.ChangeAlterColumn
	ChangeCreateIndex = plan.ChangeCreateIndex
	ChangeDropIndex   = plan.ChangeDropIndex
	ChangeAddFK              = plan.ChangeAddFK
	ChangeDropFK             = plan.ChangeDropFK
	ChangeValidateConstraint = plan.ChangeValidateConstraint
)

// HazardCode constants
const (
	HazardDropTable        = plan.HazardDropTable
	HazardDropColumn       = plan.HazardDropColumn
	HazardTypeNarrow       = plan.HazardTypeNarrow
	HazardNotNullNoDefault = plan.HazardNotNullNoDefault
	HazardIndexBuild       = plan.HazardIndexBuild
	HazardDropIndex        = plan.HazardDropIndex
	HazardDropFK           = plan.HazardDropFK
)

// HazardLevel constants
const (
	HazardLevelCritical = plan.HazardLevelCritical
	HazardLevelWarning  = plan.HazardLevelWarning
	HazardLevelNotice   = plan.HazardLevelNotice
)

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
	colorBold   = "\033[1m"
	colorCyan   = "\033[36m"
)

// GenerateLockID produces a deterministic 64-bit integer lock ID from db and schema names.
func GenerateLockID(dbName, schemaName string) int64 {
	return postgres.GenerateLockID(dbName + ":" + schemaName)
}

func detectDialect(ctx context.Context, db *sql.DB) (Dialect, error) {
	if db == nil {
		return "", fmt.Errorf("grizzle: database connection is nil")
	}

	if drv := db.Driver(); drv != nil {
		drvName := strings.ToLower(fmt.Sprintf("%T", drv))
		switch {
		case strings.Contains(drvName, "sqlite"):
			return DialectSQLite, nil
		case strings.Contains(drvName, "pgx"), strings.Contains(drvName, "pq"), strings.Contains(drvName, "postgres"):
			return DialectPostgres, nil
		}
	}

	var sqliteVer string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&sqliteVer); err == nil {
		return DialectSQLite, nil
	}

	var pgVer string
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&pgVer); err == nil {
		return DialectPostgres, nil
	}

	return "", fmt.Errorf("grizzle: unable to detect database dialect, please set Options.Dialect explicitly")
}

func prepareOptions(ctx context.Context, db *sql.DB, opts *Options) error {
	if strings.TrimSpace(opts.SchemaSQL) == "" {
		return ErrEmptySchema
	}
	if opts.StrictScope && len(opts.IncludeTables) == 0 {
		return ErrStrictScope
	}
	if opts.Dialect == DialectAuto {
		d, err := detectDialect(ctx, db)
		if err != nil {
			return err
		}
		opts.Dialect = d
	}
	switch opts.Dialect {
	case DialectSQLite:
		opts.TargetSchema = cmp.Or(opts.TargetSchema, "main")
	case DialectPostgres:
		opts.TargetSchema = cmp.Or(opts.TargetSchema, "public")
		opts.ShadowSchema = cmp.Or(opts.ShadowSchema, "_grizzle_shadow")
		if opts.LockID == 0 {
			opts.LockID = defaultPostgresLockID(opts.TargetSchema)
		}
		if opts.LockTimeout <= 0 {
			opts.LockTimeout = exec.DefaultLockTimeout
		}
		if opts.StatementTimeout <= 0 {
			opts.StatementTimeout = exec.DefaultStatementTimeout
		}
		if opts.MaxRetries < 0 {
			opts.MaxRetries = 0
		} else if opts.MaxRetries == 0 {
			opts.MaxRetries = exec.DefaultMaxRetries
		}
	default:
		return fmt.Errorf("grizzle: unsupported dialect %q", opts.Dialect)
	}
	return nil
}

// Sync synchronizes the target database schema to match the desired state in opts.SchemaSQL.
func Sync(ctx context.Context, db *sql.DB, opts Options) error {
	if err := prepareOptions(ctx, db, &opts); err != nil {
		return err
	}

	policy := resolveDropPolicy(opts)
	filters := toScopeFilters(opts)

	if opts.Dialect == DialectSQLite {
		return exec.SyncSQLite(ctx, db, exec.SQLiteExecConfig{
			SchemaSQL:     opts.SchemaSQL,
			Filters:       filters,
			Policy:        policy,
			AcceptHazards: opts.AcceptHazards,
			ExpectedHash:  opts.ExpectedHash,
			Logger:        opts.Logger,
			DryRun:        opts.DryRun,
		})
	}

	return exec.SyncPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema:         opts.TargetSchema,
		ShadowSchema:         opts.ShadowSchema,
		SchemaSQL:            opts.SchemaSQL,
		LockID:               opts.LockID,
		Filters:              filters,
		Policy:               policy,
		AcceptHazards:        opts.AcceptHazards,
		ExpectedHash:         opts.ExpectedHash,
		NonConcurrentIndexes: opts.NonConcurrentIndexes,
		LockTimeout:          opts.LockTimeout,
		StatementTimeout:     opts.StatementTimeout,
		MaxRetries:           opts.MaxRetries,
		RandFloat:            opts.RandFloat,
		Logger:               opts.Logger,
		DryRun:               opts.DryRun,
	})
}

// PlanDiff inspects the live database and computes the planned migration steps without applying them.
func PlanDiff(ctx context.Context, db *sql.DB, opts Options) (*Plan, error) {
	if err := prepareOptions(ctx, db, &opts); err != nil {
		return nil, err
	}

	policy := resolveDropPolicy(opts)
	filters := toScopeFilters(opts)

	if opts.Dialect == DialectSQLite {
		return exec.PlanDiffSQLite(ctx, db, exec.SQLiteExecConfig{
			SchemaSQL: opts.SchemaSQL,
			Filters:   filters,
			Policy:    policy,
			Logger:    opts.Logger,
			DryRun:    opts.DryRun,
		})
	}

	return exec.PlanDiffPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema:         opts.TargetSchema,
		ShadowSchema:         opts.ShadowSchema,
		SchemaSQL:            opts.SchemaSQL,
		LockID:               opts.LockID,
		Filters:              filters,
		Policy:               policy,
		NonConcurrentIndexes: opts.NonConcurrentIndexes,
		LockTimeout:          opts.LockTimeout,
		StatementTimeout:     opts.StatementTimeout,
		MaxRetries:           opts.MaxRetries,
		RandFloat:            opts.RandFloat,
		Logger:               opts.Logger,
		DryRun:               opts.DryRun,
	})
}

// ApplyOpts configures verification and execution parameters when applying an approved Plan.
type ApplyOpts struct {
	// ExpectedHash is the approved plan hash. If non-empty, Apply recomputes the plan after
	// acquiring the lock and verifies the recomputed hash matches ExpectedHash.
	// If it does not match, Apply aborts with ErrPlanDrift.
	ExpectedHash string

	// AcceptHazards specifies explicitly accepted critical hazards.
	AcceptHazards []HazardCode
}

// Apply applies an approved migration plan to the database.
// If ExpectedHash is provided and the plan recomputed post-lock differs, Apply aborts with ErrPlanDrift.
func Apply(ctx context.Context, db *sql.DB, p *Plan, opts ApplyOpts) error {
	if p == nil {
		return fmt.Errorf("grizzle: plan cannot be nil")
	}
	if db == nil {
		return fmt.Errorf("grizzle: database connection is nil")
	}
	if opts.ExpectedHash != "" && p.Hash() != opts.ExpectedHash {
		return fmt.Errorf("%w: plan hash %q does not match expected hash %q", ErrPlanDrift, p.Hash(), opts.ExpectedHash)
	}

	if p.SchemaSQL != "" {
		syncOpts := Options{
			SchemaSQL:       p.SchemaSQL,
			TargetSchema:    p.TargetSchema,
			IncludeTables:   p.IncludeTables,
			ExcludeTables:   p.ExcludeTables,
			AllowDropTable:  &p.Policy.AllowTable,
			AllowDropColumn: &p.Policy.AllowColumn,
			AllowDropIndex:  &p.Policy.AllowIndex,
			AllowDropFK:     &p.Policy.AllowFK,
			AcceptHazards:   opts.AcceptHazards,
			ExpectedHash:    opts.ExpectedHash,
		}
		return Sync(ctx, db, syncOpts)
	}

	// Direct execution fallback if SchemaSQL was not retained
	if err := exec.GateHazards(p, opts.AcceptHazards); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range p.Steps {
		if !p.Policy.IsAllowed(s) {
			return &plan.DestructiveViolationError{Violations: []plan.Step{s}}
		}
		if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
			return fmt.Errorf("%w: failed executing [%s]: %v", plan.ErrExecutionFailed, s.SQL, err)
		}
	}
	return tx.Commit()
}

// tableFilters bridges internal scope filters for existing root test suites.
type tableFilters struct {
	includes []string
	excludes []string
}

func (tf tableFilters) toScope() scope.Filters {
	return scope.Filters{
		Includes: tf.includes,
		Excludes: tf.excludes,
	}
}

// diffSchemas is an internal test helper preserving backward compatibility for root-level tests.
func diffSchemas(live, desired *schema.Schema, targetSchema, shadowSchema string, filters ...any) []plan.Step {
	var f scope.Filters
	if len(filters) > 0 {
		if tf, ok := filters[0].(tableFilters); ok {
			f = tf.toScope()
		} else if sf, ok := filters[0].(scope.Filters); ok {
			f = sf
		}
	}
	changes := diff.Diff(live, desired, targetSchema, shadowSchema, f)
	return postgres.RenderChanges(targetSchema, changes)
}

// diffSQLiteSchemas is an internal test helper preserving backward compatibility for root-level tests.
func diffSQLiteSchemas(live, desired *schema.Schema, filters ...any) []plan.Step {
	var f scope.Filters
	if len(filters) > 0 {
		if tf, ok := filters[0].(tableFilters); ok {
			f = tf.toScope()
		} else if sf, ok := filters[0].(scope.Filters); ok {
			f = sf
		}
	}
	return sqlite.Diff(live, desired, f)
}

// normalizeType is an internal test helper preserving backward compatibility.
func normalizeType(raw string) string {
	return schema.NormalizeType(raw)
}

// normalizeDefault is an internal test helper preserving backward compatibility.
func normalizeDefault(raw string) string {
	return schema.NormalizeDefault(raw)
}

// normalizeDefinition is an internal test helper preserving backward compatibility.
func normalizeDefinition(def, shadowSchema, targetSchema string) string {
	return schema.NormalizeDefinition(def, shadowSchema, targetSchema)
}
