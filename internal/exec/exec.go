package exec

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yourorg/grizzle/internal/dialect/postgres"
	"github.com/yourorg/grizzle/internal/dialect/sqlite"
	"github.com/yourorg/grizzle/internal/diff"
	"github.com/yourorg/grizzle/internal/plan"
	"github.com/yourorg/grizzle/internal/scope"
)

// PostgresExecConfig specifies the execution options for PostgreSQL synchronization.
type PostgresExecConfig struct {
	TargetSchema  string
	ShadowSchema  string
	SchemaSQL     string
	LockID        int64
	Filters       scope.Filters
	Policy        plan.DropPolicy
	AcceptHazards []plan.HazardCode
	ExpectedHash  string
	Logger        *slog.Logger
	DryRun        bool
}

// DiffPostgres computes the diff and renders the sequenced migration steps for PostgreSQL.
func DiffPostgres(ctx context.Context, tx *sql.Tx, cfg PostgresExecConfig) ([]plan.Step, error) {
	live, err := postgres.Inspect(ctx, tx, cfg.TargetSchema)
	if err != nil {
		return nil, fmt.Errorf("%w: live schema: %v", plan.ErrInspectionFailed, err)
	}

	desired, err := postgres.Inspect(ctx, tx, cfg.ShadowSchema)
	if err != nil {
		return nil, fmt.Errorf("%w: shadow schema: %v", plan.ErrInspectionFailed, err)
	}

	changes := diff.Diff(live, desired, cfg.TargetSchema, cfg.ShadowSchema, cfg.Filters)
	steps := postgres.RenderChanges(cfg.TargetSchema, changes)
	return steps, nil
}

// SyncPostgres synchronizes PostgreSQL within a single transaction protected by an advisory lock.
func SyncPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) error {
	start := time.Now()
	logger := cfg.Logger
	if logger != nil {
		logger.InfoContext(ctx, "grizzle: starting schema synchronization", "target_schema", cfg.TargetSchema)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("grizzle: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Acquire transaction advisory lock
	if err := postgres.AcquireAdvisoryLock(ctx, tx, cfg.LockID); err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: lock acquisition failed", "lock_id", cfg.LockID, "error", err)
		}
		return fmt.Errorf("%w: %v", plan.ErrLockAcquisition, err)
	}
	if logger != nil {
		logger.DebugContext(ctx, "grizzle: acquired advisory lock", "lock_id", cfg.LockID)
	}

	// 2. Setup shadow schema and ensure cleanup
	if err := postgres.SetupShadowSchema(ctx, tx, cfg.ShadowSchema); err != nil {
		return err
	}
	defer func() { _ = postgres.DropShadowSchema(context.Background(), tx, cfg.ShadowSchema) }()

	// 3. Compile user schema in shadow schema
	shadowStart := time.Now()
	if err := postgres.RunShadowDDL(ctx, tx, cfg.ShadowSchema, cfg.TargetSchema, cfg.SchemaSQL); err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: shadow compilation failed", "error", err)
		}
		return fmt.Errorf("%w: %v", plan.ErrCompilationFailed, err)
	}
	if logger != nil {
		logger.DebugContext(ctx, "grizzle: shadow compilation succeeded", "duration", time.Since(shadowStart))
	}

	// 4. Diff schemas
	steps, err := DiffPostgres(ctx, tx, cfg)
	if err != nil {
		return err
	}

	p := &plan.Plan{
		TargetSchema:  cfg.TargetSchema,
		Steps:         steps,
		Policy:        cfg.Policy,
		IncludeTables: cfg.Filters.Includes,
		ExcludeTables: cfg.Filters.Excludes,
		SchemaSQL:     cfg.SchemaSQL,
	}

	// 4b. Verify expected plan hash if provided (aborts with ErrPlanDrift on mismatch)
	if cfg.ExpectedHash != "" && p.Hash() != cfg.ExpectedHash {
		if logger != nil {
			logger.WarnContext(ctx, "grizzle: plan drift detected post-lock", "expected", cfg.ExpectedHash, "actual", p.Hash())
		}
		return fmt.Errorf("%w: expected hash %q, actual post-lock hash %q", plan.ErrPlanDrift, cfg.ExpectedHash, p.Hash())
	}

	if len(steps) == 0 {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: schema is already in sync", "duration", time.Since(start))
		}
		_ = postgres.DropShadowSchema(ctx, tx, cfg.ShadowSchema)
		return tx.Commit()
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: computed migration plan", "steps_count", len(steps))
	}

	// 5. Enforce safety policy (policy check hard-blocks drops regardless of AcceptHazards)
	var violations []plan.Step
	for _, s := range steps {
		if !cfg.Policy.IsAllowed(s) {
			violations = append(violations, s)
		}
	}
	if len(violations) > 0 {
		if logger != nil {
			logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "violations_count", len(violations))
		}
		return &plan.DestructiveViolationError{Violations: violations}
	}

	// 5b. Enforce hazard gating (fails on critical hazards unless explicitly accepted)
	if err := GateHazards(p, cfg.AcceptHazards); err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "grizzle: migration blocked by unaccepted critical hazards", "error", err)
		}
		return err
	}

	if cfg.DryRun {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: dry-run mode, skipping statement execution")
		}
		_ = postgres.DropShadowSchema(ctx, tx, cfg.ShadowSchema)
		return tx.Commit()
	}

	// 6. Apply DDL statements
	for i, s := range steps {
		stepStart := time.Now()
		if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
			if logger != nil {
				logger.ErrorContext(ctx, "grizzle: failed executing step", "step_index", i+1, "sql", s.SQL, "error", err)
			}
			return fmt.Errorf("%w: failed executing [%s]: %v", plan.ErrExecutionFailed, s.SQL, err)
		}
		if logger != nil {
			logger.DebugContext(ctx, "grizzle: executed step", "step_index", i+1, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
		}
	}

	// 7. Cleanup shadow schema
	if err := postgres.DropShadowSchema(ctx, tx, cfg.ShadowSchema); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: synchronization finished successfully", "steps_applied", len(steps), "total_duration", time.Since(start))
	}

	return nil
}

// PlanDiffPostgres generates the plan for PostgreSQL without applying statements.
func PlanDiffPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) (*plan.Plan, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := postgres.SetupShadowSchema(ctx, tx, cfg.ShadowSchema); err != nil {
		return nil, err
	}
	defer func() { _ = postgres.DropShadowSchema(context.Background(), tx, cfg.ShadowSchema) }()

	if err := postgres.RunShadowDDL(ctx, tx, cfg.ShadowSchema, cfg.TargetSchema, cfg.SchemaSQL); err != nil {
		return nil, fmt.Errorf("%w: %v", plan.ErrCompilationFailed, err)
	}

	steps, err := DiffPostgres(ctx, tx, cfg)
	if err != nil {
		return nil, err
	}

	return &plan.Plan{
		TargetSchema:  cfg.TargetSchema,
		Steps:         steps,
		Policy:        cfg.Policy,
		IncludeTables: cfg.Filters.Includes,
		ExcludeTables: cfg.Filters.Excludes,
		SchemaSQL:     cfg.SchemaSQL,
	}, nil
}

// SQLiteExecConfig specifies the execution options for SQLite synchronization.
type SQLiteExecConfig struct {
	SchemaSQL     string
	Filters       scope.Filters
	Policy        plan.DropPolicy
	AcceptHazards []plan.HazardCode
	ExpectedHash  string
	Logger        *slog.Logger
	DryRun        bool
}

// SyncSQLite synchronizes SQLite in a single transaction with foreign keys handling.
func SyncSQLite(ctx context.Context, db *sql.DB, cfg SQLiteExecConfig) error {
	start := time.Now()
	logger := cfg.Logger
	if logger != nil {
		logger.InfoContext(ctx, "sqlite: starting SQLite schema synchronization")
	}

	// 1. Compile in shadow database
	desired, err := sqlite.CompileInShadow(ctx, cfg.SchemaSQL)
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "sqlite: shadow compilation failed", "error", err)
		}
		return fmt.Errorf("%w: %v", plan.ErrCompilationFailed, err)
	}

	// 2. Introspect live schema
	live, err := sqlite.Inspect(ctx, db)
	if err != nil {
		return fmt.Errorf("%w: %v", plan.ErrInspectionFailed, err)
	}

	// 3. Diff schemas
	steps := sqlite.Diff(live, desired, cfg.Filters)

	p := &plan.Plan{
		TargetSchema:  "main",
		Steps:         steps,
		Policy:        cfg.Policy,
		IncludeTables: cfg.Filters.Includes,
		ExcludeTables: cfg.Filters.Excludes,
		SchemaSQL:     cfg.SchemaSQL,
	}

	// 3b. Verify expected plan hash if provided (aborts with ErrPlanDrift on mismatch)
	if cfg.ExpectedHash != "" && p.Hash() != cfg.ExpectedHash {
		if logger != nil {
			logger.WarnContext(ctx, "sqlite: plan drift detected post-lock", "expected", cfg.ExpectedHash, "actual", p.Hash())
		}
		return fmt.Errorf("%w: expected hash %q, actual post-lock hash %q", plan.ErrPlanDrift, cfg.ExpectedHash, p.Hash())
	}

	if len(steps) == 0 {
		if logger != nil {
			logger.InfoContext(ctx, "sqlite: schema is already in sync", "duration", time.Since(start))
		}
		return nil
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: computed migration plan", "steps_count", len(steps))
	}

	// 4. Enforce safety policy (policy check hard-blocks drops regardless of AcceptHazards)
	var violations []plan.Step
	for _, s := range steps {
		if !cfg.Policy.IsAllowed(s) {
			violations = append(violations, s)
		}
	}
	if len(violations) > 0 {
		if logger != nil {
			logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "violations_count", len(violations))
		}
		return &plan.DestructiveViolationError{Violations: violations}
	}

	// 4b. Enforce hazard gating (fails on critical hazards unless explicitly accepted)
	if err := GateHazards(p, cfg.AcceptHazards); err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "sqlite: migration blocked by unaccepted critical hazards", "error", err)
		}
		return err
	}

	if cfg.DryRun {
		return nil
	}

	// 5. Execute in transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "PRAGMA foreign_keys = OFF;"); err != nil {
		return fmt.Errorf("sqlite: failed to disable foreign keys: %w", err)
	}

	for i, s := range steps {
		stepStart := time.Now()
		sqlToExec := strings.TrimSpace(s.SQL)
		if sqlToExec == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, sqlToExec); err != nil {
			if logger != nil {
				logger.ErrorContext(ctx, "sqlite: failed executing step", "step_index", i+1, "sql", sqlToExec, "error", err)
			}
			return fmt.Errorf("%w: failed executing [%s]: %v", plan.ErrExecutionFailed, sqlToExec, err)
		}
		if logger != nil {
			logger.DebugContext(ctx, "sqlite: executed step", "step_index", i+1, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
		}
	}

	if _, err := tx.ExecContext(ctx, "PRAGMA foreign_key_check;"); err != nil {
		return fmt.Errorf("sqlite: foreign key check failed: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA foreign_keys = ON;"); err != nil {
		return fmt.Errorf("sqlite: failed to re-enable foreign keys: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: synchronization finished successfully", "steps_applied", len(steps), "total_duration", time.Since(start))
	}

	return nil
}

// PlanDiffSQLite generates the migration plan for SQLite without applying it.
func PlanDiffSQLite(ctx context.Context, db *sql.DB, cfg SQLiteExecConfig) (*plan.Plan, error) {
	desired, err := sqlite.CompileInShadow(ctx, cfg.SchemaSQL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", plan.ErrCompilationFailed, err)
	}

	live, err := sqlite.Inspect(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", plan.ErrInspectionFailed, err)
	}

	steps := sqlite.Diff(live, desired, cfg.Filters)

	return &plan.Plan{
		TargetSchema:  "main",
		Steps:         steps,
		Policy:        cfg.Policy,
		IncludeTables: cfg.Filters.Includes,
		ExcludeTables: cfg.Filters.Excludes,
		SchemaSQL:     cfg.SchemaSQL,
	}, nil
}

// GateHazards checks whether any critical hazards in the plan are not accepted.
func GateHazards(p *plan.Plan, accept []plan.HazardCode) error {
	var unaccepted []plan.Hazard
	for _, h := range p.Hazards() {
		if h.Level == plan.HazardLevelCritical {
			if !slices.Contains(accept, h.Code) {
				unaccepted = append(unaccepted, h)
			}
		}
	}
	if len(unaccepted) > 0 {
		return &plan.HazardError{Hazards: unaccepted}
	}
	return nil
}

