package exec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/dialect/sqlite"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/history"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
)

// PostgresExecConfig specifies the execution options for PostgreSQL synchronization.
type PostgresExecConfig struct {
	TargetSchema         string
	TargetSchemas        []string
	ShadowSchema         string
	ShadowSchemas        []string
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
	Backfill             BackfillFunc
}

func (cfg PostgresExecConfig) targetSchemas() []string {
	if len(cfg.TargetSchemas) > 0 {
		return cfg.TargetSchemas
	}
	if cfg.TargetSchema != "" {
		return []string{cfg.TargetSchema}
	}
	return []string{"public"}
}

func (cfg PostgresExecConfig) primarySchema() string {
	return cfg.targetSchemas()[0]
}

func searchPathSQL(schemas []string) string {
	var parts []string
	for _, s := range schemas {
		if err := postgres.ValidateIdentifier(s); err == nil {
			parts = append(parts, fmt.Sprintf("%q", s))
		}
	}
	parts = append(parts, "public")
	return fmt.Sprintf("SET search_path TO %s;", strings.Join(parts, ", "))
}

func localSearchPathSQL(schemas []string) string {
	var parts []string
	for _, s := range schemas {
		if err := postgres.ValidateIdentifier(s); err == nil {
			parts = append(parts, fmt.Sprintf("%q", s))
		}
	}
	parts = append(parts, "public")
	return fmt.Sprintf("SET LOCAL search_path TO %s;", strings.Join(parts, ", "))
}

// DiffPostgres computes the diff and renders the sequenced migration steps for PostgreSQL.
func DiffPostgres(ctx context.Context, dbtx dialect.DBTX, cfg PostgresExecConfig) ([]plan.Step, error) {
	targetSchemas := cfg.targetSchemas()
	if len(targetSchemas) <= 1 {
		targetSchema := cfg.primarySchema()
		shadowSchema := cfg.ShadowSchema
		if shadowSchema == "" {
			shadowSchema = "_grizzle_shadow"
		}

		live, err := postgres.Inspect(ctx, dbtx, targetSchema)
		if err != nil {
			return nil, fmt.Errorf("%w: live schema: %w", plan.ErrInspectionFailed, err)
		}

		desired, err := postgres.Inspect(ctx, dbtx, shadowSchema)
		if err != nil {
			return nil, fmt.Errorf("%w: shadow schema: %w", plan.ErrInspectionFailed, err)
		}

		changes, err := diff.Diff(live, desired, targetSchema, shadowSchema, cfg.Filters)
		if err != nil {
			return nil, err
		}
		steps := postgres.RenderChanges(targetSchema, changes, cfg.NonConcurrentIndexes)
		return steps, nil
	}

	// Multi-schema diffing
	shadowMap := postgres.ComputeShadowSchemas(cfg.ShadowSchema, targetSchemas)
	shadowToTarget := make(map[string]string, len(shadowMap))
	var shadowSchemas []string
	for target, shadow := range shadowMap {
		shadowToTarget[shadow] = target
		shadowSchemas = append(shadowSchemas, shadow)
	}

	liveMap, err := postgres.InspectSchemas(ctx, dbtx, targetSchemas)
	if err != nil {
		return nil, fmt.Errorf("%w: live schemas: %w", plan.ErrInspectionFailed, err)
	}

	desiredMap, err := postgres.InspectSchemas(ctx, dbtx, shadowSchemas)
	if err != nil {
		return nil, fmt.Errorf("%w: shadow schemas: %w", plan.ErrInspectionFailed, err)
	}

	var allChanges []diff.Change
	for _, target := range targetSchemas {
		shadow := shadowMap[target]
		live := liveMap[target]
		desired := desiredMap[shadow]
		changes, err := diff.DiffWithMappings(live, desired, target, shadow, cfg.Filters, shadowToTarget)
		if err != nil {
			return nil, err
		}
		allChanges = append(allChanges, changes...)
	}

	steps := postgres.RenderChanges(targetSchemas[0], allChanges, cfg.NonConcurrentIndexes)
	return steps, nil
}

// StepGroup partitions contiguous steps into transactional and non-transactional execution batches.
type StepGroup struct {
	NonTx bool
	Steps []plan.Step
}

// GroupSteps partitions migration steps into contiguous batches based on transactional requirement.
// Non-transactional steps run standalone. VALIDATE CONSTRAINT steps run in separate transactions
// after ADD ... NOT VALID steps commit, releasing ACCESS EXCLUSIVE table locks.
func GroupSteps(steps []plan.Step) []StepGroup {
	var groups []StepGroup
	for _, s := range steps {
		isNewGroup := len(groups) == 0 ||
			groups[len(groups)-1].NonTx != s.NonTx ||
			s.Type == plan.ChangeValidateConstraint ||
			(len(groups) > 0 && len(groups[len(groups)-1].Steps) > 0 && groups[len(groups)-1].Steps[len(groups[len(groups)-1].Steps)-1].Type == plan.ChangeValidateConstraint)

		if isNewGroup {
			groups = append(groups, StepGroup{NonTx: s.NonTx, Steps: []plan.Step{s}})
		} else {
			groups[len(groups)-1].Steps = append(groups[len(groups)-1].Steps, s)
		}
	}
	return groups
}

// SyncPostgres synchronizes PostgreSQL with session-level advisory locking, timeouts, and retry logic.
func SyncPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) error {
	maxRetries := max(cfg.MaxRetries, 0)

	attempt := 0
	for {
		committed, err := syncPostgresOnce(ctx, db, cfg)
		if err == nil {
			if cfg.Backfill != nil && cfg.Filters.ExpandContract {
				if err := RunBackfill(ctx, db, cfg.TargetSchema, cfg.Filters.Renames, nil, cfg.Backfill, cfg.Logger); err != nil {
					return err
				}
			}
			return nil
		}

		// Only retry before any step has committed. After partial progress: abort, require re-plan.
		if committed > 0 || !IsLockTimeout(err) || attempt >= maxRetries {
			return err
		}

		attempt++
		backoff := ComputeBackoff(attempt, cfg.RandFloat)
		if cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "grizzle: lock timeout encountered before progress, retrying migration",
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

func syncPostgresOnce(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) (int, error) {
	start := time.Now()
	logger := cfg.Logger
	targetSchemas := cfg.targetSchemas()
	primarySchema := cfg.primarySchema()
	isMulti := len(targetSchemas) > 1

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: starting schema synchronization", "target_schemas", targetSchemas)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("grizzle: failed to acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	for _, s := range targetSchemas {
		if err := postgres.ValidateIdentifier(s); err == nil && s != "public" {
			_, _ = conn.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %q;", s))
		}
	}
	_, _ = conn.ExecContext(ctx, searchPathSQL(targetSchemas))

	// Apply session-level timeouts
	_ = ApplySessionTimeouts(ctx, conn, cfg.LockTimeout, cfg.StatementTimeout)

	// 1. Acquire session-level advisory lock on dedicated connection
	lockTimeout := cfg.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = DefaultLockTimeout
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, lockTimeout)
	err = postgres.AcquireSessionAdvisoryLock(lockCtx, conn, cfg.LockID)
	cancelLock()
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: lock acquisition failed", "lock_id", cfg.LockID, "error", err)
		}
		return 0, fmt.Errorf("%w: %w", plan.ErrLockAcquisition, err)
	}
	defer func() {
		_ = postgres.ReleaseSessionAdvisoryLock(context.Background(), conn, cfg.LockID)
		_, _ = conn.ExecContext(context.Background(), "RESET search_path; RESET lock_timeout; RESET statement_timeout;")
	}()

	if logger != nil {
		logger.DebugContext(ctx, "grizzle: acquired session advisory lock", "lock_id", cfg.LockID)
	}

	// 2. Setup shadow schema and diff schemas inside an isolated transaction
	shadowStart := time.Now()
	shadowTx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("grizzle: failed to begin shadow tx: %w", err)
	}
	defer func() { _ = shadowTx.Rollback() }()

	_ = ApplyTxTimeouts(ctx, shadowTx, cfg.LockTimeout, cfg.StatementTimeout)

	var shadowSchemas []string
	if !isMulti {
		shadowSchema := cfg.ShadowSchema
		if shadowSchema == "" {
			shadowSchema = "_grizzle_shadow"
		}
		shadowSchemas = []string{shadowSchema}

		if err := postgres.SetupShadowSchema(ctx, shadowTx, shadowSchema); err != nil {
			return 0, err
		}

		if err := postgres.RunShadowDDL(ctx, shadowTx, shadowSchema, primarySchema, cfg.SchemaSQL); err != nil {
			if logger != nil {
				logger.ErrorContext(ctx, "grizzle: shadow compilation failed", "error", err)
			}
			return 0, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	} else {
		shadowMap := postgres.ComputeShadowSchemas(cfg.ShadowSchema, targetSchemas)
		for _, shadow := range shadowMap {
			shadowSchemas = append(shadowSchemas, shadow)
		}

		if err := postgres.SetupShadowSchemas(ctx, shadowTx, shadowSchemas); err != nil {
			return 0, err
		}

		if err := postgres.RunMultiShadowDDL(ctx, shadowTx, shadowMap, targetSchemas, cfg.SchemaSQL); err != nil {
			if logger != nil {
				logger.ErrorContext(ctx, "grizzle: shadow compilation failed", "error", err)
			}
			return 0, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	}
	if logger != nil {
		logger.DebugContext(ctx, "grizzle: shadow compilation succeeded", "duration", time.Since(shadowStart))
	}

	// 3. Diff schemas post-lock
	steps, err := DiffPostgres(ctx, shadowTx, cfg)
	if err != nil {
		return 0, err
	}
	_ = shadowTx.Rollback()

	p := &plan.Plan{
		TargetSchema:   primarySchema,
		TargetSchemas:  targetSchemas,
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
		return 0, fmt.Errorf("%w: expected hash %q, actual post-lock hash %q", plan.ErrPlanDrift, cfg.ExpectedHash, p.Hash())
	}

	if len(steps) == 0 {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: schema is already in sync", "duration", time.Since(start))
		}
		_ = postgres.DropShadowSchemas(ctx, conn, shadowSchemas)
		return 0, nil
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: computed migration plan", "steps_count", len(steps))
	}

	// 5. Enforce safety policy (policy check hard-blocks drops regardless of AcceptHazards)
	if err := p.ValidatePolicy(); err != nil {
		if logger != nil {
			if vErr, ok := errors.AsType[*plan.DestructiveViolationError](err); ok {
				logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "violations_count", len(vErr.Violations))
			} else {
				logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "error", err)
			}
		}
		return 0, err
	}

	// 5b. Enforce hazard gating (fails on critical hazards unless explicitly accepted)
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "grizzle: migration blocked by unaccepted critical hazards", "error", err)
		}
		return 0, err
	}

	if cfg.DryRun {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: dry-run mode, skipping statement execution")
		}
		_ = postgres.DropShadowSchemas(ctx, conn, shadowSchemas)
		return 0, nil
	}

	// 6. Cleanup shadow schema before live execution
	if err := postgres.DropShadowSchemas(ctx, conn, shadowSchemas); err != nil {
		return 0, err
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
			defer func() { _ = histConn.Close() }()
			_ = history.RecordProgress(histCtx, histConn, "postgres", primarySchema, p, status, failedStep, execErr, time.Since(start))
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
					return committedSteps + 1, fmt.Errorf("%w: failed executing non-tx [%s]: %w", plan.ErrExecutionFailed, s.SQL, err)
				}
				committedSteps++
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed non-tx step", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}
			if isLastGroup {
				if err := history.RecordPlan(ctx, conn, "postgres", primarySchema, p, time.Since(start)); err != nil {
					if logger != nil {
						logger.WarnContext(ctx, "grizzle: failed recording history on conn", "error", err)
					}
				}
			}
		} else {
			// Transactional group executed in transaction on dedicated conn
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return committedSteps, fmt.Errorf("grizzle: failed to begin step transaction: %w", err)
			}
			_, _ = tx.ExecContext(ctx, localSearchPathSQL(targetSchemas))
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
					return committedSteps, fmt.Errorf("%w: failed executing [%s]: %w", plan.ErrExecutionFailed, s.SQL, err)
				}
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed step in tx", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}

			// Record history in same transaction before commit where possible
			if isLastGroup {
				if err := history.RecordPlan(ctx, tx, "postgres", primarySchema, p, time.Since(start)); err != nil {
					if logger != nil {
						logger.WarnContext(ctx, "grizzle: failed recording history in tx", "error", err)
					}
				}
			}

			if err := tx.Commit(); err != nil {
				recordFailureHistory(stepIdx, err, false)
				return committedSteps, fmt.Errorf("grizzle: failed committing step transaction: %w", err)
			}
			committedSteps += len(group.Steps)
		}
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: synchronization finished successfully", "steps_applied", len(steps), "total_duration", time.Since(start))
	}

	return committedSteps, nil
}

// PlanDiffPostgres generates the plan for PostgreSQL without applying statements.
func PlanDiffPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) (*plan.Plan, error) {
	targetSchemas := cfg.targetSchemas()
	primarySchema := cfg.primarySchema()
	isMulti := len(targetSchemas) > 1

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_ = ApplyTxTimeouts(ctx, tx, cfg.LockTimeout, cfg.StatementTimeout)

	var shadowSchemas []string
	if !isMulti {
		shadowSchema := cfg.ShadowSchema
		if shadowSchema == "" {
			shadowSchema = "_grizzle_shadow"
		}
		shadowSchemas = []string{shadowSchema}

		if err := postgres.SetupShadowSchema(ctx, tx, shadowSchema); err != nil {
			return nil, err
		}
		defer func() { _ = postgres.DropShadowSchema(context.Background(), tx, shadowSchema) }()

		if err := postgres.RunShadowDDL(ctx, tx, shadowSchema, primarySchema, cfg.SchemaSQL); err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	} else {
		shadowMap := postgres.ComputeShadowSchemas(cfg.ShadowSchema, targetSchemas)
		for _, shadow := range shadowMap {
			shadowSchemas = append(shadowSchemas, shadow)
		}

		if err := postgres.SetupShadowSchemas(ctx, tx, shadowSchemas); err != nil {
			return nil, err
		}
		defer func() { _ = postgres.DropShadowSchemas(context.Background(), tx, shadowSchemas) }()

		if err := postgres.RunMultiShadowDDL(ctx, tx, shadowMap, targetSchemas, cfg.SchemaSQL); err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	}

	steps, err := DiffPostgres(ctx, tx, cfg)
	if err != nil {
		return nil, err
	}

	return &plan.Plan{
		TargetSchema:   primarySchema,
		TargetSchemas:  targetSchemas,
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
	Backfill      BackfillFunc
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
		return fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
	}

	// 2. Introspect live schema
	live, err := sqlite.Inspect(ctx, db)
	if err != nil {
		return fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
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
	if err := p.ValidatePolicy(); err != nil {
		if logger != nil {
			if vErr, ok := errors.AsType[*plan.DestructiveViolationError](err); ok {
				logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "violations_count", len(vErr.Violations))
			} else {
				logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "error", err)
			}
		}
		return err
	}

	// 4b. Enforce hazard gating (fails on critical hazards unless explicitly accepted)
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
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
			return fmt.Errorf("%w: failed executing [%s]: %w", plan.ErrExecutionFailed, sqlToExec, err)
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

	if cfg.Backfill != nil && cfg.Filters.ExpandContract {
		if err := RunBackfill(ctx, db, "main", cfg.Filters.Renames, steps, cfg.Backfill, cfg.Logger); err != nil {
			return err
		}
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
		return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
	}

	live, err := sqlite.Inspect(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
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

// ApplyPostgres applies a precomputed plan to PostgreSQL using a dedicated connection,
// session-level advisory locking, step grouping (NonTx vs Tx), timeouts, and retry logic.
func ApplyPostgres(ctx context.Context, db *sql.DB, p *plan.Plan, cfg PostgresExecConfig) error {
	if p == nil {
		return fmt.Errorf("grizzle: plan cannot be nil")
	}

	// 1. Enforce safety policy (policy check hard-blocks drops regardless of AcceptHazards)
	if err := p.ValidatePolicy(); err != nil {
		if cfg.Logger != nil {
			if vErr, ok := errors.AsType[*plan.DestructiveViolationError](err); ok {
				cfg.Logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "violations_count", len(vErr.Violations))
			} else {
				cfg.Logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "error", err)
			}
		}
		return err
	}

	// 1b. Enforce hazard gating (fails on critical hazards unless explicitly accepted)
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		if cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "grizzle: migration blocked by unaccepted critical hazards", "error", err)
		}
		return err
	}

	if cfg.DryRun || len(p.Steps) == 0 {
		return nil
	}

	maxRetries := max(cfg.MaxRetries, 0)
	attempt := 0

	for {
		committed, err := applyPostgresOnce(ctx, db, p, cfg)
		if err == nil {
			if cfg.Backfill != nil && p.ExpandContract {
				if err := RunBackfill(ctx, db, cfg.TargetSchema, p.Renames, nil, cfg.Backfill, cfg.Logger); err != nil {
					return err
				}
			}
			return nil
		}

		// Only retry before any step has committed. After partial progress: abort, require re-plan.
		if committed > 0 || !IsLockTimeout(err) || attempt >= maxRetries {
			return err
		}

		attempt++
		backoff := ComputeBackoff(attempt, cfg.RandFloat)
		if cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "grizzle: lock timeout encountered before progress, retrying plan apply",
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

func applyPostgresOnce(ctx context.Context, db *sql.DB, p *plan.Plan, cfg PostgresExecConfig) (int, error) {
	start := time.Now()
	logger := cfg.Logger
	targetSchemas := cfg.targetSchemas()
	primarySchema := cfg.primarySchema()

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: starting plan execution", "target_schemas", targetSchemas, "steps_count", len(p.Steps))
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("grizzle: failed to acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	for _, s := range targetSchemas {
		if err := postgres.ValidateIdentifier(s); err == nil && s != "public" {
			_, _ = conn.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %q;", s))
		}
	}
	_, _ = conn.ExecContext(ctx, searchPathSQL(targetSchemas))

	// Apply session-level timeouts
	_ = ApplySessionTimeouts(ctx, conn, cfg.LockTimeout, cfg.StatementTimeout)

	// Acquire session-level advisory lock on dedicated connection
	lockTimeout := cfg.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = DefaultLockTimeout
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, lockTimeout)
	err = postgres.AcquireSessionAdvisoryLock(lockCtx, conn, cfg.LockID)
	cancelLock()
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: lock acquisition failed", "lock_id", cfg.LockID, "error", err)
		}
		return 0, fmt.Errorf("%w: %w", plan.ErrLockAcquisition, err)
	}
	defer func() {
		_ = postgres.ReleaseSessionAdvisoryLock(context.Background(), conn, cfg.LockID)
		_, _ = conn.ExecContext(context.Background(), "RESET search_path; RESET lock_timeout; RESET statement_timeout;")
	}()

	if logger != nil {
		logger.DebugContext(ctx, "grizzle: acquired session advisory lock", "lock_id", cfg.LockID)
	}

	// Idempotency: if this plan has already been applied by another process under the lock, skip execution.
	if history.IsApplied(ctx, conn, "postgres", primarySchema, p.Hash()) {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: plan already applied by another process, skipping", "plan_hash", p.Hash())
		}
		return 0, nil
	}

	// Apply DDL statements split into transactional and non-transactional groups
	groups := GroupSteps(p.Steps)
	stepIdx := 0
	committedSteps := 0

	recordFailureHistory := func(failedStep int, execErr error, isNonTx bool) {
		status := "failed"
		if committedSteps > 0 || isNonTx {
			status = "partial"
		}
		histCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		histConn, err := db.Conn(histCtx)
		if err == nil {
			defer func() { _ = histConn.Close() }()
			_ = history.RecordProgress(histCtx, histConn, "postgres", primarySchema, p, status, failedStep, execErr, time.Since(start))
		}
	}

	for groupIdx, group := range groups {
		isLastGroup := groupIdx == len(groups)-1
		if group.NonTx {
			for _, s := range group.Steps {
				stepIdx++
				stepStart := time.Now()
				if _, err := conn.ExecContext(ctx, s.SQL); err != nil {
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed executing non-tx step", "step_index", stepIdx, "sql", s.SQL, "error", err)
					}
					recordFailureHistory(stepIdx, err, true)
					return committedSteps + 1, fmt.Errorf("%w: failed executing non-tx [%s]: %w", plan.ErrExecutionFailed, s.SQL, err)
				}
				committedSteps++
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed non-tx step", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}
			if isLastGroup {
				if err := history.RecordPlan(ctx, conn, "postgres", primarySchema, p, time.Since(start)); err != nil {
					if logger != nil {
						logger.WarnContext(ctx, "grizzle: failed recording history on conn", "error", err)
					}
				}
			}
		} else {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return committedSteps, fmt.Errorf("grizzle: failed to begin step transaction: %w", err)
			}
			_, _ = tx.ExecContext(ctx, localSearchPathSQL(targetSchemas))
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
					return committedSteps, fmt.Errorf("%w: failed executing [%s]: %w", plan.ErrExecutionFailed, s.SQL, err)
				}
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed step in tx", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}

			if isLastGroup {
				if err := history.RecordPlan(ctx, tx, "postgres", primarySchema, p, time.Since(start)); err != nil {
					if logger != nil {
						logger.WarnContext(ctx, "grizzle: failed recording history in tx", "error", err)
					}
				}
			}

			if err := tx.Commit(); err != nil {
				recordFailureHistory(stepIdx, err, false)
				return committedSteps, fmt.Errorf("grizzle: failed committing step transaction: %w", err)
			}
			committedSteps += len(group.Steps)
		}
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: plan execution finished successfully", "steps_applied", len(p.Steps), "total_duration", time.Since(start))
	}

	return committedSteps, nil
}

// ApplySQLite applies a precomputed plan to SQLite in a single transaction with foreign keys handling.
func ApplySQLite(ctx context.Context, db *sql.DB, p *plan.Plan, cfg SQLiteExecConfig) error {
	if p == nil {
		return fmt.Errorf("sqlite: plan cannot be nil")
	}

	if err := p.ValidatePolicy(); err != nil {
		if cfg.Logger != nil {
			if vErr, ok := errors.AsType[*plan.DestructiveViolationError](err); ok {
				cfg.Logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "violations_count", len(vErr.Violations))
			} else {
				cfg.Logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "error", err)
			}
		}
		return err
	}

	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		if cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "sqlite: migration blocked by unaccepted critical hazards", "error", err)
		}
		return err
	}

	if cfg.DryRun || len(p.Steps) == 0 {
		return nil
	}

	start := time.Now()
	logger := cfg.Logger
	if logger != nil {
		logger.InfoContext(ctx, "sqlite: starting SQLite plan application")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency: if this plan has already been applied by another process, skip execution.
	if history.IsApplied(ctx, tx, "sqlite", "", p.Hash()) {
		if logger != nil {
			logger.InfoContext(ctx, "sqlite: plan already applied by another process, skipping", "plan_hash", p.Hash())
		}
		return nil
	}

	if _, err := tx.ExecContext(ctx, "PRAGMA foreign_keys = OFF;"); err != nil {
		return fmt.Errorf("sqlite: failed to disable foreign keys: %w", err)
	}

	for i, s := range p.Steps {
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
			return fmt.Errorf("%w: failed executing [%s]: %w", plan.ErrExecutionFailed, sqlToExec, err)
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

	if err := history.RecordPlan(ctx, tx, "sqlite", "", p, time.Since(start)); err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "sqlite: failed recording history in tx", "error", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if cfg.Backfill != nil && p.ExpandContract {
		if err := RunBackfill(ctx, db, "main", p.Renames, p.Steps, cfg.Backfill, cfg.Logger); err != nil {
			return err
		}
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: plan application finished successfully", "steps_applied", len(p.Steps), "total_duration", time.Since(start))
	}

	return nil
}
