package grizzle

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/export"
	"github.com/muandane/grizzle/internal/lint"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
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

// CheckConstraintIR represents the intermediate representation of a CHECK constraint.
type CheckConstraintIR = schema.CheckConstraint

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
	ChangeAddCheck           = plan.ChangeAddCheck
	ChangeDropCheck          = plan.ChangeDropCheck
	ChangeValidateConstraint = plan.ChangeValidateConstraint
	ChangeRenameColumn       = plan.ChangeRenameColumn
	ChangeAttachPartition    = plan.ChangeAttachPartition
	ChangeDetachPartition    = plan.ChangeDetachPartition
)

// HazardCode constants
const (
	HazardDropTable              = plan.HazardDropTable
	HazardDropColumn             = plan.HazardDropColumn
	HazardTypeNarrow             = plan.HazardTypeNarrow
	HazardNotNullNoDefault       = plan.HazardNotNullNoDefault
	HazardIndexBuild             = plan.HazardIndexBuild
	HazardDropIndex              = plan.HazardDropIndex
	HazardDropFK                 = plan.HazardDropFK
	HazardDropCheck              = plan.HazardDropCheck
	HazardCheckValidateScan      = plan.HazardCheckValidateScan
	HazardRenameAmbiguous        = plan.HazardRenameAmbiguous
	HazardUnmanagedDependency    = plan.HazardUnmanagedDependency
	HazardGeneratedRewrite       = plan.HazardGeneratedRewrite
	HazardPartitionAttachScan    = plan.HazardPartitionAttachScan
	HazardPartitionPendingDetach = plan.HazardPartitionPendingDetach
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

// detectDialectFromDriver maps a database/sql driver's concrete type to a
// Grizzle dialect by matching the driver's package path (via reflection), not
// the %T type string, which omits package paths and misclassifies wrapper
// drivers. Pointer indirection is stripped. Returns DialectAuto when the
// driver type is unknown (wrappers such as otelsql/sqlx), letting the caller
// fall back to probe queries.
func detectDialectFromDriver(drv driver.Driver) Dialect {
	if drv == nil {
		return DialectAuto
	}
	t := reflect.TypeOf(drv)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Name() == "" {
		return DialectAuto
	}
	name := strings.ToLower(t.PkgPath() + "." + t.Name())
	switch {
	case strings.Contains(name, "github.com/jackc/pgx"),
		strings.Contains(name, "github.com/lib/pq"):
		return DialectPostgres
	case strings.Contains(name, "modernc.org/sqlite"),
		strings.Contains(name, "github.com/mattn/go-sqlite3"):
		return DialectSQLite
	default:
		return DialectAuto
	}
}

func detectDialect(ctx context.Context, db *sql.DB) (Dialect, error) {
	if db == nil {
		return "", fmt.Errorf("grizzle: database connection is nil")
	}

	if d := detectDialectFromDriver(db.Driver()); d != DialectAuto {
		return d, nil
	}

	// Wrapper drivers (otel/sqlx etc.) hide the underlying driver type; probe
	// the engine with a single query each. These are expected to fail on the
	// wrong engine and produce no log noise.
	var sqliteVer string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&sqliteVer); err == nil {
		return DialectSQLite, nil
	}

	var pgVer string
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&pgVer); err == nil {
		return DialectPostgres, nil
	}

	return "", fmt.Errorf("grizzle: unable to detect database dialect from driver %T, please set Options.Dialect explicitly", db.Driver())
}

func prepareOptions(ctx context.Context, db *sql.DB, opts *Options) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if opts.Dialect == DialectAuto {
		d, err := detectDialect(ctx, db)
		if err != nil {
			return err
		}
		opts.Dialect = d
		// Re-validate dialect-specific constraints now that dialect is known.
		if err := opts.Validate(); err != nil {
			return err
		}
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
		opts.LockNamespace = cmp.Or(opts.LockNamespace, "grizzle")
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
		if opts.LockTimeout <= 0 {
			opts.LockTimeout = exec.DefaultLockTimeout
		}
		if opts.StatementTimeout <= 0 {
			opts.StatementTimeout = exec.DefaultStatementTimeout
		}
		if opts.MaxRetries == 0 {
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

	var syncErr error
	if opts.Dialect == DialectSQLite {
		syncErr = exec.SyncSQLite(ctx, db, exec.SQLiteExecConfig{
			SchemaSQL:        opts.SchemaSQL,
			Filters:          filters,
			Policy:           policy,
			AcceptHazards:    opts.AcceptHazards,
			Logger:           opts.Logger,
			Tracer:           opts.Tracer,
			DryRun:           opts.DryRun,
			Backfill:         toExecBackfill(opts.Backfill),
			BeforeSync:       opts.BeforeSync,
			AfterSync:        opts.AfterSync,
			BeforeStep:       opts.BeforeStep,
			AfterStep:        opts.AfterStep,
			RebuildThreshold: opts.SQLiteRebuildThreshold,
			RebuildBatchSize: opts.SQLiteRebuildBatchSize,
		})
	} else {
		syncErr = exec.SyncPostgres(ctx, db, exec.PostgresExecConfig{
			TargetSchema:         opts.TargetSchema,
			TargetSchemas:        opts.TargetSchemas,
			ShadowSchema:         opts.ShadowSchema,
			SchemaSQL:            opts.SchemaSQL,
			LockNamespace:        opts.LockNamespace,
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
			Tracer:               opts.Tracer,
			DryRun:               opts.DryRun,
			Backfill:             toExecBackfill(opts.Backfill),
			BeforeSync:           opts.BeforeSync,
			AfterSync:            opts.AfterSync,
			BeforeStep:           opts.BeforeStep,
			AfterStep:            opts.AfterStep,
		})
	}
	if syncErr != nil {
		return syncErr
	}

	// Sync DDL → AfterSync (inside exec) → Seed. Skipped in dry-run mode.
	if strings.TrimSpace(opts.SeedSQL) != "" && !opts.DryRun {
		return seedWithOptions(ctx, db, &opts)
	}
	return nil
}

// Seed executes idempotent seed SQL against the database. The seed runs in a
// single transaction: either the whole script commits with an 'applied'
// grizzle_history record keyed by the seed's content hash, or nothing
// persists. If the same seed hash was already applied and force is false,
// the seed is skipped. Forced re-runs execute the script again, so seed
// scripts should tolerate re-execution (IF NOT EXISTS, ON CONFLICT...).
//
// Dialect and target schema are taken from opts (auto-detected when unset).
func Seed(ctx context.Context, db *sql.DB, seedSQL string, opts ...Option) error {
	if strings.TrimSpace(seedSQL) == "" {
		return fmt.Errorf("grizzle: seed SQL is empty")
	}
	o := Options{SeedSQL: seedSQL}
	for _, opt := range opts {
		opt(&o)
	}
	return seedWithOptions(ctx, db, &o)
}

// SeedHash returns the deterministic sha256 identity of a seed script.
// Leading and trailing whitespace is ignored so reformatted-but-identical
// seeds do not re-run.
func SeedHash(seedSQL string) string {
	return exec.SeedHash(seedSQL)
}

// seedWithOptions dispatches seed execution based on the resolved dialect.
func seedWithOptions(ctx context.Context, db *sql.DB, opts *Options) error {
	if opts.Dialect == DialectAuto {
		d, err := detectDialect(ctx, db)
		if err != nil {
			return err
		}
		opts.Dialect = d
	}
	if opts.Dialect == DialectSQLite {
		return exec.SeedSQLite(ctx, db, exec.SeedExecConfig{
			Force:  opts.SeedForce,
			Logger: opts.Logger,
		}, opts.SeedSQL)
	}
	return exec.SeedPostgres(ctx, db, exec.SeedExecConfig{
		TargetSchemas:    opts.TargetSchemas,
		LockID:           opts.LockID,
		LockNamespace:    opts.LockNamespace,
		LockTimeout:      opts.LockTimeout,
		StatementTimeout: opts.StatementTimeout,
		Force:            opts.SeedForce,
		Logger:           opts.Logger,
	}, opts.SeedSQL)
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
			Tracer:    opts.Tracer,
			DryRun:    opts.DryRun,
		})
	}

	return exec.PlanDiffPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema:         opts.TargetSchema,
		TargetSchemas:        opts.TargetSchemas,
		ShadowSchema:         opts.ShadowSchema,
		SchemaSQL:            opts.SchemaSQL,
		LockNamespace:        opts.LockNamespace,
		LockID:               opts.LockID,
		Filters:              filters,
		Policy:               policy,
		NonConcurrentIndexes: opts.NonConcurrentIndexes,
		LockTimeout:          opts.LockTimeout,
		StatementTimeout:     opts.StatementTimeout,
		MaxRetries:           opts.MaxRetries,
		RandFloat:            opts.RandFloat,
		Logger:               opts.Logger,
		Tracer:               opts.Tracer,
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

	// BeforeSync runs once before any migration steps or locks are executed.
	BeforeSync SyncHook

	// AfterSync runs once after all migration steps and history recording succeed.
	AfterSync SyncHook

	// BeforeStep executes immediately prior to executing each plan step.
	BeforeStep StepHook

	// AfterStep executes immediately following the successful execution of each plan step.
	AfterStep StepHook

	// SQLiteRebuildThreshold defines the row count threshold above which SQLite table rebuilds
	// chunk data copying by keyset. Defaults to 100000.
	SQLiteRebuildThreshold int

	// SQLiteRebuildBatchSize defines the chunk size when copying data in batches during SQLite table rebuilds.
	// Defaults to 10000.
	SQLiteRebuildBatchSize int

	// LockNamespace specifies the application namespace string used for PostgreSQL advisory locking (defaults to "grizzle").
	LockNamespace string

	// Logger receives structured migration logs during Apply (defaults to no logging).
	// The plan artifact cannot carry a logger, so it is supplied here.
	Logger *slog.Logger

	// Tracer specifies an optional tracer for observing plan application.
	Tracer Tracer
}

// optionsFromPlan maps every approval-relevant plan field into Options.
// Lock identity and shadow schema names are never taken from the artifact:
// LockNamespace comes from ApplyOpts (defaulted in prepareOptions), LockID
// stays 0 so exec derives per-schema advisory locks from trusted target
// identity, and ShadowSchema is generated ephemerally at apply time.
func optionsFromPlan(p *Plan, opts ApplyOpts) Options {
	return Options{
		SchemaSQL:              p.SchemaSQL,
		TargetSchema:           p.TargetSchema,
		TargetSchemas:          p.TargetSchemas,
		IncludeTables:          p.IncludeTables,
		ExcludeTables:          p.ExcludeTables,
		Renames:                p.Renames,
		ExpandContract:         p.ExpandContract,
		AllowDropTable:         &p.Policy.AllowTable,
		AllowDropColumn:        &p.Policy.AllowColumn,
		AllowDropIndex:         &p.Policy.AllowIndex,
		AllowDropFK:            &p.Policy.AllowFK,
		AllowDropCheck:         &p.Policy.AllowCheck,
		AcceptHazards:          opts.AcceptHazards,
		Backfill:               opts.Backfill,
		BeforeSync:             opts.BeforeSync,
		AfterSync:              opts.AfterSync,
		BeforeStep:             opts.BeforeStep,
		AfterStep:              opts.AfterStep,
		SQLiteRebuildThreshold: opts.SQLiteRebuildThreshold,
		SQLiteRebuildBatchSize: opts.SQLiteRebuildBatchSize,
		LockNamespace:          opts.LockNamespace,
		LockTimeout:            p.LockTimeout,
		StatementTimeout:       p.StatementTimeout,
		NonConcurrentIndexes:   p.NonConcurrentIndexes,
		Logger:                 opts.Logger,
		Tracer:                 opts.Tracer,
	}
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

	// Operational fields come from an untrusted artifact (plan.json): reject
	// values that could corrupt execution before any connection is touched.
	if err := p.ValidateExecutionFields(); err != nil {
		return err
	}

	if p.SchemaSQL != "" {
		syncOpts := optionsFromPlan(p, opts)
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
				Tracer:           syncOpts.Tracer,
				DryRun:           syncOpts.DryRun,
				Backfill:         toExecBackfill(opts.Backfill),
				BeforeSync:       opts.BeforeSync,
				AfterSync:        opts.AfterSync,
				BeforeStep:       opts.BeforeStep,
				AfterStep:        opts.AfterStep,
				RebuildThreshold: syncOpts.SQLiteRebuildThreshold,
				RebuildBatchSize: syncOpts.SQLiteRebuildBatchSize,
			})
		}

		return exec.SyncPostgres(ctx, db, exec.PostgresExecConfig{
			TargetSchema:         syncOpts.TargetSchema,
			TargetSchemas:        syncOpts.TargetSchemas,
			ShadowSchema:         syncOpts.ShadowSchema,
			SchemaSQL:            syncOpts.SchemaSQL,
			LockNamespace:        syncOpts.LockNamespace,
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
			Tracer:               syncOpts.Tracer,
			DryRun:               syncOpts.DryRun,
			Backfill:             toExecBackfill(opts.Backfill),
		})
	}

	// Direct execution fallback if SchemaSQL was not retained: the recorded
	// steps run as-is. Enforce the plan's recorded scope (IncludeTables /
	// ExcludeTables) against every table-scoped step first — enum steps carry
	// the enum name in Table, not a table name, so they are skipped.
	planFilters := scope.Filters{Includes: p.IncludeTables, Excludes: p.ExcludeTables}
	for _, s := range p.Steps {
		if s.Type == plan.ChangeCreateEnum || s.Type == plan.ChangeAlterEnum {
			continue
		}
		if !scope.IsTableManaged(s.Table, planFilters) {
			return fmt.Errorf("%w: plan step [%s on %q] is outside the plan's recorded scope (include_tables=%v, exclude_tables=%v)",
				ErrInvalidOptions, s.Type, s.Table, p.IncludeTables, p.ExcludeTables)
		}
	}

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
			Tracer:           opts.Tracer,
			Logger:           opts.Logger,
			Backfill:         toExecBackfill(opts.Backfill),
			BeforeSync:       opts.BeforeSync,
			AfterSync:        opts.AfterSync,
			BeforeStep:       opts.BeforeStep,
			AfterStep:        opts.AfterStep,
			RebuildThreshold: opts.SQLiteRebuildThreshold,
			RebuildBatchSize: opts.SQLiteRebuildBatchSize,
		})
	case DialectPostgres:
		targetSchemas := p.TargetSchemas
		if len(targetSchemas) == 0 {
			targetSchemas = []string{cmp.Or(p.TargetSchema, "public")}
		}
		targetSchema := targetSchemas[0]
		return exec.ApplyPostgres(ctx, db, p, exec.PostgresExecConfig{
			TargetSchema:         targetSchema,
			TargetSchemas:        targetSchemas,
			LockNamespace:        cmp.Or(opts.LockNamespace, "grizzle"),
			Policy:               p.Policy,
			AcceptHazards:        opts.AcceptHazards,
			ExpectedHash:         opts.ExpectedHash,
			Tracer:               opts.Tracer,
			Logger:               opts.Logger,
			NonConcurrentIndexes: p.NonConcurrentIndexes,
			LockTimeout:          cmp.Or(p.LockTimeout, exec.DefaultLockTimeout),
			StatementTimeout:     cmp.Or(p.StatementTimeout, exec.DefaultStatementTimeout),
			MaxRetries:           exec.DefaultMaxRetries,
			Backfill:             toExecBackfill(opts.Backfill),
			BeforeSync:           opts.BeforeSync,
			AfterSync:            opts.AfterSync,
			BeforeStep:           opts.BeforeStep,
			AfterStep:            opts.AfterStep,
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

// LintDiagnostic is a single static lint finding against a schema element.
type LintDiagnostic = lint.Diagnostic

// LintSeverity indicates how strongly a lint diagnostic should block a release.
type LintSeverity = lint.Severity

// LintRule is a pure check executed against the desired schema IR.
type LintRule = lint.Rule

const (
	// LintSeverityError marks a structural anti-pattern that must be fixed.
	LintSeverityError = lint.SeverityError
	// LintSeverityWarning marks a recommendation that may be ignored deliberately.
	LintSeverityWarning = lint.SeverityWarning
	// LintSeverityInfo marks a stylistic suggestion with no correctness impact.
	LintSeverityInfo = lint.SeverityInfo
)

// CompileSchema compiles opts.SchemaSQL into the desired SchemaIR without
// diffing or executing anything. For PostgreSQL the compilation happens in the
// shadow schema inside an always-rolled-back transaction (requires a
// connection to the target database); for SQLite it happens in an in-memory
// database, making it fully offline.
func CompileSchema(ctx context.Context, db *sql.DB, opts Options) (*SchemaIR, error) {
	if strings.TrimSpace(opts.SchemaSQL) == "" {
		return nil, ErrEmptySchema
	}
	if db == nil {
		return nil, fmt.Errorf("grizzle: database connection is nil")
	}
	if opts.Dialect == DialectAuto {
		d, err := detectDialect(ctx, db)
		if err != nil {
			return nil, err
		}
		opts.Dialect = d
	}
	switch opts.Dialect {
	case DialectSQLite:
		return exec.CompileSchemaSQLite(ctx, opts.SchemaSQL)
	case DialectPostgres:
		if len(opts.TargetSchemas) > 1 {
			return nil, ErrUnsupportedMultiSchema
		}
		targetSchema := cmp.Or(opts.TargetSchema, "public")
		shadowSchema := cmp.Or(opts.ShadowSchema, "_grizzle_shadow")
		return exec.CompileSchemaPostgres(ctx, db, exec.PostgresExecConfig{
			TargetSchema: targetSchema,
			ShadowSchema: shadowSchema,
			SchemaSQL:    opts.SchemaSQL,
		})
	default:
		return nil, fmt.Errorf("grizzle: unsupported dialect %q", opts.Dialect)
	}
}

// LintSchema runs rules (defaulting to DefaultLintRules) against the compiled
// schema IR and returns deterministically sorted diagnostics.
func LintSchema(s *SchemaIR, rules ...LintRule) []LintDiagnostic {
	if len(rules) == 0 {
		rules = DefaultLintRules()
	}
	return lint.Lint(s, rules...)
}

// DefaultLintRules returns the built-in lint rule set (L001..L004).
func DefaultLintRules() []LintRule {
	return lint.DefaultRules()
}

// LintHasErrors reports whether any diagnostic has severity ERROR.
func LintHasErrors(diags []LintDiagnostic) bool {
	return lint.HasErrors(diags)
}

// DryRunResult reports the outcome of a live dry-run verification.
type DryRunResult = exec.DryRunResult

// DryRunVerifyPlan performs a live dry-run of an approved plan artifact.
// It verifies the artifact hash against ExpectedHash, honors every
// plan-carried intent field (target schemas, scope, policy, execution
// semantics), and checks that a re-diff of the live database against the
// artifact's SchemaSQL still produces the same plan hash — so a tampered
// or drifted artifact cannot silently verify a different migration.
// Runtime-only options (logger, tracer, hazards, lock namespace) come from opts.
func DryRunVerifyPlan(ctx context.Context, db *sql.DB, p *Plan, opts ApplyOpts) (*DryRunResult, error) {
	if p == nil {
		return nil, fmt.Errorf("grizzle: plan cannot be nil")
	}
	if db == nil {
		return nil, fmt.Errorf("grizzle: database connection is nil")
	}
	if opts.ExpectedHash != "" && p.Hash() != opts.ExpectedHash {
		return nil, fmt.Errorf("%w: plan hash %q does not match expected hash %q", ErrPlanDrift, p.Hash(), opts.ExpectedHash)
	}
	if err := p.ValidateExecutionFields(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.SchemaSQL) == "" {
		return nil, fmt.Errorf("grizzle: plan has no embedded schema SQL; dry-run of approved plans requires SchemaSQL")
	}

	do := optionsFromPlan(p, opts)
	// Verify approved steps: re-diff from artifact options; recomputed hash
	// must match the approved plan so we never verify a silently regenerated
	// migration with different semantics.
	recomputed, err := PlanDiff(ctx, db, do)
	if err != nil {
		return nil, err
	}
	if recomputed.Hash() != p.Hash() {
		return nil, fmt.Errorf("%w: recomputed plan hash %q does not match approved plan hash %q",
			ErrPlanDrift, recomputed.Hash(), p.Hash())
	}

	return DryRunVerify(ctx, db, do)
}

// DryRunVerify executes a live dry-run rollback against the target database:
// the planned DDL runs against real data inside transactions that are
// unconditionally rolled back, so constraints, casts, and check expressions
// are verified without persisting any change. Non-transactional steps
// (CREATE INDEX CONCURRENTLY, ALTER TYPE ... ADD VALUE, DETACH PARTITION
// CONCURRENTLY) are skipped and reported in DryRunResult.UnverifiedNonTx.
// No records are written to grizzle_history, and hooks do not run unless
// opts.ExecuteHooksInDryRun is set.
func DryRunVerify(ctx context.Context, db *sql.DB, opts Options) (*DryRunResult, error) {
	if err := prepareOptions(ctx, db, &opts); err != nil {
		return nil, err
	}

	policy := resolveDropPolicy(opts)
	filters := toScopeFilters(opts)

	if opts.Dialect == DialectSQLite {
		return exec.DryRunVerifySQLite(ctx, db, exec.SQLiteExecConfig{
			SchemaSQL:            opts.SchemaSQL,
			Filters:              filters,
			Policy:               policy,
			AcceptHazards:        opts.AcceptHazards,
			Logger:               opts.Logger,
			Tracer:               opts.Tracer,
			BeforeStep:           opts.BeforeStep,
			AfterStep:            opts.AfterStep,
			ExecuteHooksInDryRun: opts.ExecuteHooksInDryRun,
		})
	}

	return exec.DryRunVerifyPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema:         opts.TargetSchema,
		TargetSchemas:        opts.TargetSchemas,
		ShadowSchema:         opts.ShadowSchema,
		SchemaSQL:            opts.SchemaSQL,
		LockNamespace:        opts.LockNamespace,
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
		Tracer:               opts.Tracer,
		BeforeStep:           opts.BeforeStep,
		AfterStep:            opts.AfterStep,
		DryRunLockTimeout:    opts.DryRunLockTimeout,
		ExecuteHooksInDryRun: opts.ExecuteHooksInDryRun,
	})
}

// LintFormatText renders diagnostics as human-readable text.
func LintFormatText(w io.Writer, diags []LintDiagnostic) error {
	return lint.FormatText(w, diags)
}

// LintFormatJSON renders diagnostics as a JSON array.
func LintFormatJSON(w io.Writer, diags []LintDiagnostic) error {
	return lint.FormatJSON(w, diags)
}

// LintFormatGitHub renders diagnostics as GitHub Actions annotations.
func LintFormatGitHub(w io.Writer, diags []LintDiagnostic) error {
	return lint.FormatGitHub(w, diags)
}
