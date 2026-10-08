package exec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/muandane/grizzle/internal/dialect/sqlite"
	"github.com/muandane/grizzle/internal/plan"
)

// DefaultDryRunLockTimeout bounds how long dry-run DDL may wait on table
// locks before failing fast instead of queuing behind application traffic.
const DefaultDryRunLockTimeout = 2 * time.Second

// DryRunResult reports the outcome of a live dry-run verification.
type DryRunResult struct {
	// Plan is the verified plan (including skipped NonTx steps).
	Plan *plan.Plan
	// ExecutedSteps counts transactional steps that executed and rolled back cleanly.
	ExecutedSteps int
	// UnverifiedNonTx lists non-transactional steps that could not be run in
	// the rollback sandbox (e.g. CREATE INDEX CONCURRENTLY).
	UnverifiedNonTx []plan.Step
	// VerifiedTx lists transactional steps that executed inside the sandbox.
	VerifiedTx []plan.Step
	// Duration is the total wall time of the verification.
	Duration time.Duration
}

// DryRunVerifyPostgres verifies the planned DDL against live data inside an
// always-rolled-back transaction. Non-transactional steps are skipped and
// reported in UnverifiedNonTx. Every transaction is unconditionally rolled
// back, no history is recorded, and hooks do not run unless
// cfg.ExecuteHooksInDryRun is set.
func DryRunVerifyPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) (*DryRunResult, error) {
	start := time.Now()
	logger := cfg.Logger

	// 1. Compute the plan (shadow compile + diff).
	p, err := PlanDiffPostgres(ctx, db, cfg)
	if err != nil {
		return nil, err
	}

	// 2. Enforce the same safety gates as a real sync.
	if err := p.ValidatePolicy(); err != nil {
		return nil, err
	}
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		return nil, err
	}

	if len(p.Steps) == 0 {
		return &DryRunResult{Plan: p, Duration: time.Since(start)}, nil
	}

	// 3. Dedicated connection with a strict lock timeout: DDL inside the
	// rollback transaction still acquires ACCESS EXCLUSIVE locks, so fail
	// fast rather than queueing behind application transactions.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	dryLockTimeout := cfg.DryRunLockTimeout
	if dryLockTimeout <= 0 {
		dryLockTimeout = DefaultDryRunLockTimeout
	}
	_ = ApplySessionTimeouts(ctx, conn, dryLockTimeout, cfg.StatementTimeout)

	targetSchemas := cfg.targetSchemas()
	_, _ = conn.ExecContext(ctx, searchPathSQL(targetSchemas))

	result := &DryRunResult{Plan: p, UnverifiedNonTx: []plan.Step{}, VerifiedTx: []plan.Step{}}

	groups := GroupSteps(p.Steps)
	stepIdx := 0
	// All transactional steps run inside a single transaction that is
	// unconditionally rolled back at the end: later groups may depend on
	// objects created by earlier groups (real apply commits per group, but
	// the sandbox must keep everything visible until verification ends).
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to begin dry-run transaction: %w", err)
	}
	_, _ = tx.ExecContext(ctx, localSearchPathSQL(targetSchemas))
	_ = ApplyTxTimeouts(ctx, tx, dryLockTimeout, cfg.StatementTimeout)

	var verified []plan.Step
	rollback := func(cause error) (*DryRunResult, error) {
		_ = tx.Rollback()
		return nil, cause
	}
	for _, group := range groups {
		if group.NonTx {
			// NonTx steps (CREATE INDEX CONCURRENTLY etc.) cannot run inside a
			// transaction; never silently swap them for locking equivalents.
			for _, s := range group.Steps {
				stepIdx++
				result.UnverifiedNonTx = append(result.UnverifiedNonTx, s)
			}
			continue
		}

		for _, s := range group.Steps {
			stepIdx++
			if cfg.ExecuteHooksInDryRun {
				if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: stepIdx, Total: len(p.Steps), IsNonTx: false}); err != nil {
					return rollback(fmt.Errorf("dry-run verification failed at before_step hook %d: %w", stepIdx, err))
				}
			}
			if err := execStepWithTracing(ctx, tx, s, false, cfg.Tracer, roleIRFromConfig(cfg), subscriptionIRFromConfig(cfg)); err != nil {
				return rollback(fmt.Errorf("dry-run verification failed at step %d: %w", stepIdx, err))
			}
			if cfg.ExecuteHooksInDryRun {
				if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: stepIdx, Total: len(p.Steps), IsNonTx: false}); err != nil {
					return rollback(fmt.Errorf("dry-run verification failed at after_step hook %d: %w", stepIdx, err))
				}
			}
			verified = append(verified, s)
		}
	}

	// Zero mutation invariant: the whole sandbox rolls back.
	if err := tx.Rollback(); err != nil {
		return nil, fmt.Errorf("grizzle: dry-run rollback failed: %w", err)
	}
	result.VerifiedTx = append(result.VerifiedTx, verified...)
	result.ExecutedSteps = len(verified)

	result.Duration = time.Since(start)
	if logger != nil {
		logger.InfoContext(ctx, "grizzle: dry-run verification finished", "executed_steps", result.ExecutedSteps,
			"unverified_non_tx", len(result.UnverifiedNonTx), "duration", result.Duration)
	}
	return result, nil
}

// DryRunVerifySQLite verifies the planned DDL against live data inside a
// SAVEPOINT sandbox that is always rolled back. No history is recorded and
// hooks do not run unless cfg.ExecuteHooksInDryRun is set.
func DryRunVerifySQLite(ctx context.Context, db *sql.DB, cfg SQLiteExecConfig) (*DryRunResult, error) {
	start := time.Now()
	logger := cfg.Logger

	desired, err := sqlite.CompileInShadow(ctx, cfg.SchemaSQL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
	}
	live, err := sqlite.Inspect(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
	}

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

	if err := p.ValidatePolicy(); err != nil {
		return nil, err
	}
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		return nil, err
	}

	result := &DryRunResult{Plan: p, UnverifiedNonTx: []plan.Step{}, VerifiedTx: []plan.Step{}}
	if len(steps) == 0 {
		result.Duration = time.Since(start)
		return result, nil
	}

	// Run the sandbox pinned to a single connection with foreign keys
	// disabled (per-connection PRAGMA; a no-op inside a transaction), so
	// rebuild DROPs cannot cascade against live child rows.
	err = runSQLiteWithForeignKeysOff(ctx, db, func(tx *sql.Tx, conn *sql.Conn) error {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT grizzle_dryrun;"); err != nil {
			return fmt.Errorf("sqlite: failed to create dry-run savepoint: %w", err)
		}

		for i, s := range steps {
			sqlToExec := strings.TrimSpace(s.SQL)
			if sqlToExec == "" {
				continue
			}
			if cfg.ExecuteHooksInDryRun {
				if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(steps), IsNonTx: false}); err != nil {
					_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_dryrun;")
					_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_dryrun;")
					return fmt.Errorf("dry-run verification failed at before_step hook %d: %w", i+1, err)
				}
			}
			if err := executeSQLiteStep(ctx, tx, s, cfg); err != nil {
				_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_dryrun;")
				_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_dryrun;")
				return fmt.Errorf("dry-run verification failed at step %d: %w", i+1, err)
			}
			if cfg.ExecuteHooksInDryRun {
				if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(steps), IsNonTx: false}); err != nil {
					_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_dryrun;")
					_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_dryrun;")
					return fmt.Errorf("dry-run verification failed at after_step hook %d: %w", i+1, err)
				}
			}
			result.VerifiedTx = append(result.VerifiedTx, s)
			result.ExecutedSteps++
		}

		// Validate foreign key integrity against live data before discarding.
		rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check;")
		if err != nil {
			return fmt.Errorf("sqlite: foreign key check failed: %w", err)
		}
		var fkViolations []string
		for rows.Next() {
			var vTbl, vParent string
			var vRowID, vFKID int64
			if err := rows.Scan(&vTbl, &vRowID, &vParent, &vFKID); err == nil {
				fkViolations = append(fkViolations, fmt.Sprintf("table %q row %d -> %q", vTbl, vRowID, vParent))
			}
		}
		_ = rows.Close()
		if len(fkViolations) > 0 {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_dryrun;")
			_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_dryrun;")
			return fmt.Errorf("sqlite: dry-run foreign key violation: %s", strings.Join(fkViolations, "; "))
		}

		// Zero mutation invariant: discard every change.
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_dryrun;"); err != nil {
			return fmt.Errorf("sqlite: dry-run rollback failed: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_dryrun;"); err != nil {
			return fmt.Errorf("sqlite: dry-run savepoint release failed: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	result.Duration = time.Since(start)
	if logger != nil {
		logger.InfoContext(ctx, "sqlite: dry-run verification finished", "executed_steps", result.ExecutedSteps, "duration", result.Duration)
	}
	return result, nil
}
