package exec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/muandane/grizzle/internal/plan"
)

// StepTiming captures execution and lock metrics for a single planned step.
type StepTiming struct {
	Step            plan.Step     `json:"step"`
	Duration        time.Duration `json:"duration"`
	ExceededLockMax bool          `json:"exceeded_lock_max,omitempty"`
}

// SimulateResult captures the full profile of a simulated migration run.
type SimulateResult struct {
	Plan             *plan.Plan    `json:"plan"`
	TotalDuration    time.Duration `json:"total_duration"`
	StepTimings      []StepTiming  `json:"step_timings"`
	ExceededWarnings []string      `json:"exceeded_warnings,omitempty"`
}

// SimulatePostgres runs the planned DDL in an isolated rollback transaction,
// recording per-step timings and evaluating lock hold thresholds.
func SimulatePostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig, maxLockDuration time.Duration) (*SimulateResult, error) {
	p, err := PlanDiffPostgres(ctx, db, cfg)
	if err != nil {
		return nil, err
	}
	return SimulatePostgresPlan(ctx, db, p, cfg, maxLockDuration)
}

// SimulatePostgresPlan runs the given plan in an isolated rollback transaction,
// recording per-step timings and evaluating lock hold thresholds.
func SimulatePostgresPlan(ctx context.Context, db *sql.DB, p *plan.Plan, cfg PostgresExecConfig, maxLockDuration time.Duration) (res *SimulateResult, err error) {
	start := time.Now()
	defer func() {
		if cfg.Metrics != nil {
			status := "applied"
			if err != nil {
				status = "failed"
			} else if res == nil || res.Plan == nil || len(res.Plan.Steps) == 0 {
				status = "noop"
			}
			cfg.Metrics.RecordSimulationDuration(ctx, time.Since(start), "postgres", status)
		}
	}()

	if p == nil {
		return nil, fmt.Errorf("grizzle: plan cannot be nil")
	}

	if err := p.ValidatePolicy(); err != nil {
		return nil, err
	}
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		return nil, err
	}

	if len(p.Steps) == 0 {
		res = &SimulateResult{
			Plan:          p,
			TotalDuration: time.Since(start),
			StepTimings:   []StepTiming{},
		}
		return res, nil
	}

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

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to begin simulation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, _ = tx.ExecContext(ctx, localSearchPathSQL(targetSchemas))
	_ = ApplyTxTimeouts(ctx, tx, dryLockTimeout, cfg.StatementTimeout)

	groups := GroupSteps(p.Steps)
	var stepTimings []StepTiming
	var warnings []string
	stepIdx := 0

	for _, group := range groups {
		if group.NonTx {
			for _, s := range group.Steps {
				stepIdx++
				stepTimings = append(stepTimings, StepTiming{
					Step:     s,
					Duration: 0,
				})
			}
			continue
		}

		for _, s := range group.Steps {
			stepIdx++
			if cfg.ExecuteHooksInDryRun {
				if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: stepIdx, Total: len(p.Steps), IsNonTx: false}); err != nil {
					return nil, fmt.Errorf("simulation failed at before_step hook %d: %w", stepIdx, err)
				}
			}

			stepStart := time.Now()
			stepErr := execStepWithTracing(ctx, tx, s, false, cfg.Tracer, roleIRFromConfig(cfg), subscriptionIRFromConfig(cfg))
			stepDur := time.Since(stepStart)

			if stepErr != nil {
				return nil, fmt.Errorf("simulation failed at step %d (%s): %w", stepIdx, s.Type, stepErr)
			}

			if cfg.ExecuteHooksInDryRun {
				if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: stepIdx, Total: len(p.Steps), IsNonTx: false}); err != nil {
					return nil, fmt.Errorf("simulation failed at after_step hook %d: %w", stepIdx, err)
				}
			}

			exceeded := false
			if maxLockDuration > 0 && stepDur > maxLockDuration {
				exceeded = true
				target := s.Table
				if s.Column != "" {
					target = fmt.Sprintf("%s.%s", s.Table, s.Column)
				}
				msg := fmt.Sprintf("step %d (%s %s) exceeded max lock duration: took %v (threshold: %v)",
					stepIdx, s.Type, target, stepDur, maxLockDuration)
				warnings = append(warnings, msg)
			}

			stepTimings = append(stepTimings, StepTiming{
				Step:            s,
				Duration:        stepDur,
				ExceededLockMax: exceeded,
			})
		}
	}

	// Zero mutation invariant: discard every change by rolling back.
	if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
		return nil, fmt.Errorf("grizzle: simulation rollback failed: %w", err)
	}

	totalDur := time.Since(start)
	res = &SimulateResult{
		Plan:             p,
		TotalDuration:    totalDur,
		StepTimings:      stepTimings,
		ExceededWarnings: warnings,
	}

	if cfg.Logger != nil {
		cfg.Logger.InfoContext(ctx, "grizzle: simulation finished", "executed_steps", len(stepTimings), "duration", totalDur)
	}

	return res, nil
}

// SimulateSQLite runs the planned DDL against live data in an isolated SAVEPOINT sandbox
// that is always rolled back. Foreign keys are disabled on the sandbox connection to avoid
// cascade drops during table rebuilds, and foreign key integrity is checked before rollback.
func SimulateSQLite(ctx context.Context, db *sql.DB, cfg SQLiteExecConfig, maxLockDuration time.Duration) (*SimulateResult, error) {
	var p *plan.Plan
	err := withSQLiteAttachedConn(ctx, db, cfg.SQLiteAttach, func(conn *sql.Conn) error {
		var err error
		p, err = buildSQLitePlan(ctx, conn, cfg)
		return err
	})
	if err != nil {
		return nil, err
	}
	return SimulateSQLitePlan(ctx, db, p, cfg, maxLockDuration)
}

// SimulateSQLitePlan runs the given plan in a SAVEPOINT sandbox that is always rolled back.
func SimulateSQLitePlan(ctx context.Context, db *sql.DB, p *plan.Plan, cfg SQLiteExecConfig, maxLockDuration time.Duration) (res *SimulateResult, err error) {
	start := time.Now()
	defer func() {
		if cfg.Metrics != nil {
			status := "applied"
			if err != nil {
				status = "failed"
			} else if res == nil || res.Plan == nil || len(res.Plan.Steps) == 0 {
				status = "noop"
			}
			cfg.Metrics.RecordSimulationDuration(ctx, time.Since(start), "sqlite", status)
		}
	}()

	if p == nil {
		return nil, fmt.Errorf("grizzle: plan cannot be nil")
	}

	if err := p.ValidatePolicy(); err != nil {
		return nil, err
	}
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		return nil, err
	}

	steps := p.Steps
	if len(steps) == 0 {
		res = &SimulateResult{
			Plan:          p,
			TotalDuration: time.Since(start),
			StepTimings:   []StepTiming{},
		}
		return res, nil
	}

	var stepTimings []StepTiming
	var warnings []string

	err = withSQLiteAttachedConn(ctx, db, cfg.SQLiteAttach, func(conn *sql.Conn) error {
		return runSQLiteConnWithForeignKeysOff(ctx, conn, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "SAVEPOINT grizzle_sim;"); err != nil {
				return fmt.Errorf("sqlite: failed to create simulation savepoint: %w", err)
			}

			rollback := func(cause error) error {
				_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_sim;")
				_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_sim;")
				return cause
			}

			for i, s := range steps {
				if s.NonTx {
					stepTimings = append(stepTimings, StepTiming{
						Step:     s,
						Duration: 0,
					})
					continue
				}

				sqlToExec := strings.TrimSpace(s.SQL)
				if sqlToExec == "" {
					continue
				}

				if cfg.ExecuteHooksInDryRun {
					if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(steps), IsNonTx: false}); err != nil {
						return rollback(fmt.Errorf("simulation failed at before_step hook %d: %w", i+1, err))
					}
				}

				stepStart := time.Now()
				stepErr := executeSQLiteStep(ctx, tx, s, cfg)
				stepDur := time.Since(stepStart)

				if stepErr != nil {
					return rollback(fmt.Errorf("simulation failed at step %d (%s): %w", i+1, s.Type, stepErr))
				}

				if cfg.ExecuteHooksInDryRun {
					if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(steps), IsNonTx: false}); err != nil {
						return rollback(fmt.Errorf("simulation failed at after_step hook %d: %w", i+1, err))
					}
				}

				exceeded := false
				if maxLockDuration > 0 && stepDur > maxLockDuration {
					exceeded = true
					target := s.Table
					if s.Column != "" {
						target = fmt.Sprintf("%s.%s", s.Table, s.Column)
					}
					msg := fmt.Sprintf("step %d (%s %s) exceeded max lock duration: took %v (threshold: %v)",
						i+1, s.Type, target, stepDur, maxLockDuration)
					warnings = append(warnings, msg)
				}

				stepTimings = append(stepTimings, StepTiming{
					Step:            s,
					Duration:        stepDur,
					ExceededLockMax: exceeded,
				})
			}

			// Validate foreign key integrity against live data before discarding.
			rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check;")
			if err != nil {
				return rollback(fmt.Errorf("sqlite: foreign key check failed: %w", err))
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
				return rollback(fmt.Errorf("sqlite: simulation foreign key violation: %s", strings.Join(fkViolations, "; ")))
			}

			// Zero mutation invariant: discard every change.
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_sim;"); err != nil {
				return fmt.Errorf("sqlite: simulation rollback failed: %w", err)
			}
			if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_sim;"); err != nil {
				return fmt.Errorf("sqlite: simulation savepoint release failed: %w", err)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	totalDur := time.Since(start)
	res = &SimulateResult{
		Plan:             p,
		TotalDuration:    totalDur,
		StepTimings:      stepTimings,
		ExceededWarnings: warnings,
	}

	if cfg.Logger != nil {
		cfg.Logger.InfoContext(ctx, "sqlite: simulation finished", "executed_steps", len(stepTimings), "duration", totalDur)
	}

	return res, nil
}
