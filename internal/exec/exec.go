package exec

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yourorg/grizzle/internal/dialect"
	"github.com/yourorg/grizzle/internal/dialect/postgres"
	"github.com/yourorg/grizzle/internal/dialect/sqlite"
	"github.com/yourorg/grizzle/internal/diff"
	"github.com/yourorg/grizzle/internal/history"
	"github.com/yourorg/grizzle/internal/plan"
	"github.com/yourorg/grizzle/internal/scope"
)

// PostgresExecConfig specifies the execution options for PostgreSQL synchronization.
// PostgresExecConfig specifies the execution options for PostgreSQL synchronization.
type PostgresExecConfig struct {
	TargetSchema         string
	ShadowSchema         string
	SchemaSQL            string
	LockID               int64
	Filters              scope.Filters
	Policy               plan.DropPolicy
	AcceptHazards        []plan.HazardCode
	ExpectedHash         string
	NonConcurrentIndexes bool
	LockTimeout          time.Duration
	StatementTimeout     time.Duration
	MaxRetries           int
	RandFloat            func() float64
	Logger               *slog.Logger
	DryRun               bool
}

// DiffPostgres computes the diff and renders the sequenced migration steps for PostgreSQL.
func DiffPostgres(ctx context.Context, dbtx dialect.DBTX, cfg PostgresExecConfig) ([]plan.Step, error) {
	live, err := postgres.Inspect(ctx, dbtx, cfg.TargetSchema)
	if err != nil {
		return nil, fmt.Errorf("%w: live schema: %v", plan.ErrInspectionFailed, err)
	}

	desired, err := postgres.Inspect(ctx, dbtx, cfg.ShadowSchema)
	if err != nil {
		return nil, fmt.Errorf("%w: shadow schema: %v", plan.ErrInspectionFailed, err)
	}

	changes := diff.Diff(live, desired, cfg.TargetSchema, cfg.ShadowSchema, cfg.Filters)
	steps := postgres.RenderChanges(cfg.TargetSchema, changes, cfg.NonConcurrentIndexes)
	return steps, nil
}

// StepGroup partitions contiguous steps into transactional and non-transactional execution batches.
type StepGroup struct {
	NonTx bool
	Steps []plan.Step
}

// GroupSteps partitions migration steps into contiguous batches based on transactional requirement.
func GroupSteps(steps []plan.Step) []StepGroup {
	var groups []StepGroup
	for _, s := range steps {
		if len(groups) == 0 || groups[len(groups)-1].NonTx != s.NonTx {
			groups = append(groups, StepGroup{NonTx: s.NonTx, Steps: []plan.Step{s}})
		} else {
			groups[len(groups)-1].Steps = append(groups[len(groups)-1].Steps, s)
		}
	}
	return groups
}

// SyncPostgres synchronizes PostgreSQL with session-level advisory locking, timeouts, and retry logic.
func SyncPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) error {
	maxRetries := cfg.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	attempt := 0
	for {
		err := syncPostgresOnce(ctx, db, cfg)
		if err == nil {
			return nil
		}

		if !IsLockTimeout(err) || attempt >= maxRetries {
			return err
		}

		attempt++
		backoff := ComputeBackoff(attempt, cfg.RandFloat)
		if cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "grizzle: lock timeout encountered, retrying migration",
				"attempt", attempt,
				"max_retries", maxRetries,
				"backoff_ms", backoff.Milliseconds(),
				"error", err,
			)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during lock retry: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}
}

func syncPostgresOnce(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) error {
	start := time.Now()
	logger := cfg.Logger
	if logger != nil {
		logger.InfoContext(ctx, "grizzle: starting schema synchronization", "target_schema", cfg.TargetSchema)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("grizzle: failed to acquire connection: %w", err)
	}
	defer conn.Close()

	if err := postgres.ValidateIdentifier(cfg.TargetSchema); err == nil {
		_, _ = conn.ExecContext(ctx, fmt.Sprintf("SET search_path TO %q, public;", cfg.TargetSchema))
	}

	// Apply session-level timeouts
	_ = ApplySessionTimeouts(ctx, conn, cfg.LockTimeout, cfg.StatementTimeout)

	// 1. Acquire session-level advisory lock on dedicated connection
	if err := postgres.AcquireSessionAdvisoryLock(ctx, conn, cfg.LockID); err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: lock acquisition failed", "lock_id", cfg.LockID, "error", err)
		}
		return fmt.Errorf("%w: %v", plan.ErrLockAcquisition, err)
	}
	defer func() {
		_ = postgres.ReleaseSessionAdvisoryLock(context.Background(), conn, cfg.LockID)
	}()

	if logger != nil {
		logger.DebugContext(ctx, "grizzle: acquired session advisory lock", "lock_id", cfg.LockID)
	}

	// 2. Setup shadow schema and diff schemas inside an isolated transaction
	shadowStart := time.Now()
	shadowTx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("grizzle: failed to begin shadow tx: %w", err)
	}
	defer func() { _ = shadowTx.Rollback() }()

	_ = ApplyTxTimeouts(ctx, shadowTx, cfg.LockTimeout, cfg.StatementTimeout)

	if err := postgres.SetupShadowSchema(ctx, shadowTx, cfg.ShadowSchema); err != nil {
		return err
	}

	if err := postgres.RunShadowDDL(ctx, shadowTx, cfg.ShadowSchema, cfg.TargetSchema, cfg.SchemaSQL); err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: shadow compilation failed", "error", err)
		}
		return fmt.Errorf("%w: %v", plan.ErrCompilationFailed, err)
	}
	if logger != nil {
		logger.DebugContext(ctx, "grizzle: shadow compilation succeeded", "duration", time.Since(shadowStart))
	}

	// 3. Diff schemas post-lock
	steps, err := DiffPostgres(ctx, shadowTx, cfg)
	if err != nil {
		return err
	}
	_ = shadowTx.Rollback()

	p := &plan.Plan{
		TargetSchema:   cfg.TargetSchema,
		Steps:          steps,
		Policy:         cfg.Policy,
		IncludeTables:  cfg.Filters.Includes,
		ExcludeTables:  cfg.Filters.Excludes,
		Renames:        cfg.Filters.Renames,
		ExpandContract: cfg.Filters.ExpandContract,
		SchemaSQL:      cfg.SchemaSQL,
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
		_ = postgres.DropShadowSchema(ctx, conn, cfg.ShadowSchema)
		return nil
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
		_ = postgres.DropShadowSchema(ctx, conn, cfg.ShadowSchema)
		return nil
	}

	// 6. Cleanup shadow schema before live execution
	if err := postgres.DropShadowSchema(ctx, conn, cfg.ShadowSchema); err != nil {
		return err
	}

	// 7. Apply DDL statements split into transactional and non-transactional groups
	groups := GroupSteps(steps)
	stepIdx := 0
	committedSteps := 0

	recordFailureHistory := func(failedStep int, execErr error, isNonTx bool) {
		status := "failed"
		if committedSteps > 0 || isNonTx {
			status = "partial"
		}
		// Write using fresh connection and detached context not cancelled by the failure
		histCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		histConn, err := db.Conn(histCtx)
		if err == nil {
			defer histConn.Close()
			_ = history.RecordProgress(histCtx, histConn, "postgres", cfg.TargetSchema, p, status, failedStep, execErr, time.Since(start))
		}
	}

	for groupIdx, group := range groups {
		isLastGroup := groupIdx == len(groups)-1
		if group.NonTx {
			// Non-transactional steps (e.g. CREATE INDEX CONCURRENTLY) executed directly on dedicated conn
			for _, s := range group.Steps {
				stepIdx++
				stepStart := time.Now()
				if _, err := conn.ExecContext(ctx, s.SQL); err != nil {
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed executing non-tx step", "step_index", stepIdx, "sql", s.SQL, "error", err)
					}
					recordFailureHistory(stepIdx, err, true)
					return fmt.Errorf("%w: failed executing non-tx [%s]: %v", plan.ErrExecutionFailed, s.SQL, err)
				}
				committedSteps++
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed non-tx step", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}
			if isLastGroup {
				if err := history.RecordPlan(ctx, conn, "postgres", cfg.TargetSchema, p, time.Since(start)); err != nil {
					if logger != nil {
						logger.WarnContext(ctx, "grizzle: failed recording history on conn", "error", err)
					}
				}
			}
		} else {
			// Transactional group executed in transaction on dedicated conn
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("grizzle: failed to begin step transaction: %w", err)
			}
			if err := postgres.ValidateIdentifier(cfg.TargetSchema); err == nil {
				_, _ = tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL search_path TO %q, public;", cfg.TargetSchema))
			}
			_ = ApplyTxTimeouts(ctx, tx, cfg.LockTimeout, cfg.StatementTimeout)

			for _, s := range group.Steps {
				stepIdx++
				stepStart := time.Now()
				if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
					_ = tx.Rollback()
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed executing step in tx", "step_index", stepIdx, "sql", s.SQL, "error", err)
					}
					recordFailureHistory(stepIdx, err, false)
					return fmt.Errorf("%w: failed executing [%s]: %v", plan.ErrExecutionFailed, s.SQL, err)
				}
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed step in tx", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}

			// Record history in same transaction before commit where possible
			if isLastGroup {
				if err := history.RecordPlan(ctx, tx, "postgres", cfg.TargetSchema, p, time.Since(start)); err != nil {
					if logger != nil {
						logger.WarnContext(ctx, "grizzle: failed recording history in tx", "error", err)
					}
				}
			}

			if err := tx.Commit(); err != nil {
				recordFailureHistory(stepIdx, err, false)
				return fmt.Errorf("grizzle: failed committing step transaction: %w", err)
			}
			committedSteps += len(group.Steps)
		}
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

	_ = ApplyTxTimeouts(ctx, tx, cfg.LockTimeout, cfg.StatementTimeout)

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
		TargetSchema:   cfg.TargetSchema,
		Steps:          steps,
		Policy:         cfg.Policy,
		IncludeTables:  cfg.Filters.Includes,
		ExcludeTables:  cfg.Filters.Excludes,
		Renames:        cfg.Filters.Renames,
		ExpandContract: cfg.Filters.ExpandContract,
		SchemaSQL:      cfg.SchemaSQL,
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
		TargetSchema:   "main",
		Steps:          steps,
		Policy:         cfg.Policy,
		IncludeTables:  cfg.Filters.Includes,
		ExcludeTables:  cfg.Filters.Excludes,
		Renames:        cfg.Filters.Renames,
		ExpandContract: cfg.Filters.ExpandContract,
		SchemaSQL:      cfg.SchemaSQL,
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
			_ = tx.Rollback()
			if logger != nil {
				logger.ErrorContext(ctx, "sqlite: failed executing step", "step_index", i+1, "sql", sqlToExec, "error", err)
			}
			histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = history.RecordProgress(histCtx, db, "sqlite", "", p, "failed", i+1, err, time.Since(start))
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

	// Record history in same transaction
	if err := history.RecordPlan(ctx, tx, "sqlite", "", p, time.Since(start)); err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "sqlite: failed recording history in tx", "error", err)
		}
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
		TargetSchema:   "main",
		Steps:          steps,
		Policy:         cfg.Policy,
		IncludeTables:  cfg.Filters.Includes,
		ExcludeTables:  cfg.Filters.Excludes,
		Renames:        cfg.Filters.Renames,
		ExpandContract: cfg.Filters.ExpandContract,
		SchemaSQL:      cfg.SchemaSQL,
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

