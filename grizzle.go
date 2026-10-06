package grizzle

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/export"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// Plan is the complete migration plan containing sequenced steps, policy, and hashes.
type Plan = plan.Plan

// PlanDocument represents a complete, serialized migration plan artifact including hash, hazards, scope, and options digest.
type PlanDocument = plan.Document

// ScopeDocument represents the scope section of a plan document.
type ScopeDocument = plan.ScopeDocument

// Step represents a single atomic migration step to execute.
type Step = plan.Step

// ChangeType describes the classification of a schema change.
type ChangeType = plan.ChangeType

// DropPolicy defines the safety behavior for destructive drops.
type DropPolicy = plan.DropPolicy

// HazardLevel indicates the severity of potential data loss or downtime.
type HazardLevel = plan.HazardLevel

// HazardCode identifies the specific type of migration hazard detected.
type HazardCode = plan.HazardCode

// Hazard describes an operation that could cause data loss or service disruption.
type Hazard = plan.Hazard

// SchemaIR represents the database-agnostic schema intermediate representation.
type SchemaIR = schema.Schema

// TableIR represents the intermediate representation of a database table.
type TableIR = schema.Table

// ColumnIR represents the intermediate representation of a database column.
type ColumnIR = schema.Column

// IndexIR represents the intermediate representation of a database index.
type IndexIR = schema.Index

// ForeignKeyIR represents the intermediate representation of a foreign key relationship.
type ForeignKeyIR = schema.ForeignKey

// PrimaryKeyIR represents the intermediate representation of a table primary key.
type PrimaryKeyIR = schema.PrimaryKey

// EnumIR represents the intermediate representation of an enum type.
type EnumIR = schema.Enum

// GeneratedColumn describes a computed or generated column.
type GeneratedColumn = schema.GeneratedColumn

// PartitionStrategy represents the partitioning method used by a partitioned table.
type PartitionStrategy = schema.PartitionStrategy

// PartitionKey defines how a partitioned table is divided.
type PartitionKey = schema.PartitionKey

// PartitionOf defines the relationship of a partition table to its parent partitioned table.
type PartitionOf = schema.PartitionOf

// PartitionStrategy constants
const (
	// PartitionStrategyRange indicates range-based partitioning.
	PartitionStrategyRange = schema.PartitionStrategyRange
	// PartitionStrategyList indicates list-based partitioning.
	PartitionStrategyList = schema.PartitionStrategyList
	// PartitionStrategyHash indicates hash-based partitioning.
	PartitionStrategyHash = schema.PartitionStrategyHash
)

// ChangeType constants
const (
	ChangeCreateEnum         = plan.ChangeCreateEnum
	ChangeAlterEnum          = plan.ChangeAlterEnum
	ChangeCreateTable        = plan.ChangeCreateTable
	ChangeDropTable          = plan.ChangeDropTable
	ChangeAddColumn          = plan.ChangeAddColumn
	ChangeDropColumn         = plan.ChangeDropColumn
	ChangeAlterColumn        = plan.ChangeAlterColumn
	ChangeCreateIndex        = plan.ChangeCreateIndex
	ChangeDropIndex          = plan.ChangeDropIndex
	ChangeAddFK              = plan.ChangeAddFK
	ChangeDropFK             = plan.ChangeDropFK
	ChangeValidateConstraint = plan.ChangeValidateConstraint
	ChangeRenameColumn       = plan.ChangeRenameColumn
	ChangeAttachPartition    = plan.ChangeAttachPartition
	ChangeDetachPartition    = plan.ChangeDetachPartition
)

// HazardCode constants
const (
	HazardDropTable           = plan.HazardDropTable
	HazardDropColumn          = plan.HazardDropColumn
	HazardTypeNarrow          = plan.HazardTypeNarrow
	HazardNotNullNoDefault    = plan.HazardNotNullNoDefault
	HazardIndexBuild          = plan.HazardIndexBuild
	HazardDropIndex           = plan.HazardDropIndex
	HazardDropFK              = plan.HazardDropFK
	HazardRenameAmbiguous     = plan.HazardRenameAmbiguous
	HazardUnmanagedDependency = plan.HazardUnmanagedDependency
	HazardGeneratedRewrite    = plan.HazardGeneratedRewrite
	HazardPartitionAttachScan = plan.HazardPartitionAttachScan
)

// UnmanagedObject represents an unmanaged database object detected during introspection.
type UnmanagedObject = schema.UnmanagedObject

// UnmanagedKind represents the category of an unmanaged database object.
type UnmanagedKind = schema.UnmanagedKind

// DependencyRef represents a reference to a table or column that an unmanaged object depends on.
type DependencyRef = schema.DependencyRef

const (
	// UnmanagedView indicates a standard SQL VIEW.
	UnmanagedView = schema.UnmanagedView
	// UnmanagedMaterialized indicates a MATERIALIZED VIEW.
	UnmanagedMaterialized = schema.UnmanagedMaterialized
	// UnmanagedTrigger indicates a database trigger attached to a table.
	UnmanagedTrigger = schema.UnmanagedTrigger
	// UnmanagedFunction indicates a stored procedure or function.
	UnmanagedFunction = schema.UnmanagedFunction
	// UnmanagedSequence indicates an unmanaged database sequence.
	UnmanagedSequence = schema.UnmanagedSequence
	// UnmanagedEnum indicates an unmanaged custom enum type.
	UnmanagedEnum = schema.UnmanagedEnum
	// UnmanagedDomain indicates an unmanaged domain type.
	UnmanagedDomain = schema.UnmanagedDomain
)

// HazardLevel constants
const (
	HazardLevelCritical = plan.HazardLevelCritical
	HazardLevelWarning  = plan.HazardLevelWarning
	HazardLevelNotice   = plan.HazardLevelNotice
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
		if len(opts.TargetSchemas) == 0 {
			opts.TargetSchemas = []string{opts.TargetSchema}
		}
		if opts.SQLiteRebuildThreshold == 0 {
			opts.SQLiteRebuildThreshold = 100000
		}
		if opts.SQLiteRebuildBatchSize <= 0 {
			opts.SQLiteRebuildBatchSize = 10000
		}
	case DialectPostgres:
		if len(opts.TargetSchemas) > 0 {
			slices.Sort(opts.TargetSchemas)
			opts.TargetSchemas = slices.Compact(opts.TargetSchemas)
			if opts.TargetSchema == "" {
				opts.TargetSchema = opts.TargetSchemas[0]
			}
		} else if opts.TargetSchema != "" {
			opts.TargetSchemas = []string{opts.TargetSchema}
		} else {
			opts.TargetSchema = "public"
			opts.TargetSchemas = []string{"public"}
		}
		opts.ShadowSchema = cmp.Or(opts.ShadowSchema, "_grizzle_shadow")
		if opts.LockID == 0 {
			opts.LockID = defaultPostgresLockIDFromSchemas(opts.TargetSchemas)
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
			SchemaSQL:        opts.SchemaSQL,
			Filters:          filters,
			Policy:           policy,
			AcceptHazards:    opts.AcceptHazards,
			Logger:           opts.Logger,
			DryRun:           opts.DryRun,
			Backfill:         toExecBackfill(opts.Backfill),
			RebuildThreshold: opts.SQLiteRebuildThreshold,
			RebuildBatchSize: opts.SQLiteRebuildBatchSize,
		})
	}

	return exec.SyncPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema:         opts.TargetSchema,
		TargetSchemas:        opts.TargetSchemas,
		ShadowSchema:         opts.ShadowSchema,
		SchemaSQL:            opts.SchemaSQL,
		LockID:               opts.LockID,
		Filters:              filters,
		Policy:               policy,
		AcceptHazards:        opts.AcceptHazards,
		NonConcurrentIndexes: opts.NonConcurrentIndexes,
		LockTimeout:          opts.LockTimeout,
		StatementTimeout:     opts.StatementTimeout,
		MaxRetries:           opts.MaxRetries,
		RandFloat:            opts.RandFloat,
		Logger:               opts.Logger,
		DryRun:               opts.DryRun,
		Backfill:             toExecBackfill(opts.Backfill),
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
		TargetSchemas:        opts.TargetSchemas,
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

	// Backfill hook function run outside the DDL lock window in batches during staged expand migration.
	Backfill BackfillFunc

	// SQLiteRebuildThreshold defines the row count threshold above which SQLite table rebuilds
	// chunk data copying by keyset. Defaults to 100000.
	SQLiteRebuildThreshold int

	// SQLiteRebuildBatchSize defines the chunk size when copying data in batches during SQLite table rebuilds.
	// Defaults to 10000.
	SQLiteRebuildBatchSize int
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
			TargetSchemas:   p.TargetSchemas,
			IncludeTables:   p.IncludeTables,
			ExcludeTables:   p.ExcludeTables,
			Renames:         p.Renames,
			ExpandContract:  p.ExpandContract,
			AllowDropTable:  &p.Policy.AllowTable,
			AllowDropColumn: &p.Policy.AllowColumn,
			AllowDropIndex:  &p.Policy.AllowIndex,
			AllowDropFK:     &p.Policy.AllowFK,
			AcceptHazards:          opts.AcceptHazards,
			Backfill:               opts.Backfill,
			SQLiteRebuildThreshold: opts.SQLiteRebuildThreshold,
			SQLiteRebuildBatchSize: opts.SQLiteRebuildBatchSize,
		}
		if err := prepareOptions(ctx, db, &syncOpts); err != nil {
			return err
		}
		policy := resolveDropPolicy(syncOpts)
		filters := toScopeFilters(syncOpts)

		if syncOpts.Dialect == DialectSQLite {
			return exec.SyncSQLite(ctx, db, exec.SQLiteExecConfig{
				SchemaSQL:        syncOpts.SchemaSQL,
				Filters:          filters,
				Policy:           policy,
				AcceptHazards:    opts.AcceptHazards,
				ExpectedHash:     opts.ExpectedHash,
				Logger:           syncOpts.Logger,
				DryRun:           syncOpts.DryRun,
				Backfill:         toExecBackfill(opts.Backfill),
				RebuildThreshold: syncOpts.SQLiteRebuildThreshold,
				RebuildBatchSize: syncOpts.SQLiteRebuildBatchSize,
			})
		}

		return exec.SyncPostgres(ctx, db, exec.PostgresExecConfig{
			TargetSchema:         syncOpts.TargetSchema,
			TargetSchemas:        syncOpts.TargetSchemas,
			ShadowSchema:         syncOpts.ShadowSchema,
			SchemaSQL:            syncOpts.SchemaSQL,
			LockID:               syncOpts.LockID,
			Filters:              filters,
			Policy:               policy,
			AcceptHazards:        opts.AcceptHazards,
			ExpectedHash:         opts.ExpectedHash,
			NonConcurrentIndexes: syncOpts.NonConcurrentIndexes,
			LockTimeout:          syncOpts.LockTimeout,
			StatementTimeout:     syncOpts.StatementTimeout,
			MaxRetries:           syncOpts.MaxRetries,
			RandFloat:            syncOpts.RandFloat,
			Logger:               syncOpts.Logger,
			DryRun:               syncOpts.DryRun,
			Backfill:             toExecBackfill(opts.Backfill),
		})
	}

	// Direct execution fallback if SchemaSQL was not retained
	dialect, err := detectDialect(ctx, db)
	if err != nil {
		return err
	}

	switch dialect {
	case DialectSQLite:
		return exec.ApplySQLite(ctx, db, p, exec.SQLiteExecConfig{
			Policy:           p.Policy,
			AcceptHazards:    opts.AcceptHazards,
			ExpectedHash:     opts.ExpectedHash,
			Backfill:         toExecBackfill(opts.Backfill),
			RebuildThreshold: opts.SQLiteRebuildThreshold,
			RebuildBatchSize: opts.SQLiteRebuildBatchSize,
		})
	case DialectPostgres:
		targetSchemas := p.TargetSchemas
		if len(targetSchemas) == 0 {
			targetSchemas = []string{cmp.Or(p.TargetSchema, "public")}
		}
		targetSchema := targetSchemas[0]
		lockID := defaultPostgresLockIDFromSchemas(targetSchemas)
		return exec.ApplyPostgres(ctx, db, p, exec.PostgresExecConfig{
			TargetSchema:     targetSchema,
			TargetSchemas:    targetSchemas,
			LockID:           lockID,
			Policy:           p.Policy,
			AcceptHazards:    opts.AcceptHazards,
			ExpectedHash:     opts.ExpectedHash,
			LockTimeout:      exec.DefaultLockTimeout,
			StatementTimeout: exec.DefaultStatementTimeout,
			MaxRetries:       exec.DefaultMaxRetries,
			Backfill:         toExecBackfill(opts.Backfill),
		})
	default:
		return fmt.Errorf("grizzle: unsupported dialect %q", dialect)
	}
}

// Check inspects the live database and returns ErrDrift (wrapped in DriftError) if the schema
// differs from the desired schema specified in opts.SchemaSQL.
// It is strictly read-only, never modifies the database, and never auto-fixes drift.
func Check(ctx context.Context, db *sql.DB, opts Options) error {
	p, err := PlanDiff(ctx, db, opts)
	if err != nil {
		return err
	}

	if len(p.Steps) > 0 {
		return &plan.DriftError{Plan: p}
	}

	return nil
}

func toExecBackfill(fn BackfillFunc) exec.BackfillFunc {
	if fn == nil {
		return nil
	}
	return exec.BackfillFunc(fn)
}

// ExportFormat defines supported target formats for migration export.
type ExportFormat = export.Format

const (
	// ExportFormatSQL targets raw SQL files with transaction markers.
	ExportFormatSQL = export.FormatSQL
	// ExportFormatGoose targets Goose-compatible migration files.
	ExportFormatGoose = export.FormatGoose
	// ExportFormatAtlas targets Atlas-compatible migration files.
	ExportFormatAtlas = export.FormatAtlas
)

// ExportArtifact represents a generated migration file ready to be saved.
type ExportArtifact = export.Artifact

// Export serializes the given plan into migration files for the requested tool format.
func Export(p *Plan, format ExportFormat, version string, timestamp ...time.Time) ([]ExportArtifact, error) {
	var ts time.Time
	if len(timestamp) > 0 {
		ts = timestamp[0]
	}
	return export.Export(p, format, version, ts)
}

// ParsePlanJSON parses a serialized plan JSON string or bytes into a *Plan, returning the recorded plan hash.
func ParsePlanJSON(data []byte) (*Plan, string, error) {
	return plan.ParsePlanJSON(data)
}
