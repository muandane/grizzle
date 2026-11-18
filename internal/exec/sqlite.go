package exec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/muandane/grizzle/internal/history"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
)

// SQLiteExecConfig specifies the execution options for SQLite synchronization.
type SQLiteExecConfig struct {
	SchemaSQL            string
	TargetSchema         string
	TargetSchemas        []string
	SQLiteAttach         map[string]string
	Filters              scope.Filters
	Policy               plan.DropPolicy
	AcceptHazards        []plan.HazardCode
	AllowResumePartial   bool
	ExpectedHash         string
	Logger               *slog.Logger
	Tracer               Tracer
	Metrics              Metrics
	DryRun               bool
	Backfill             BackfillFunc
	BeforeSync           SyncHook
	AfterSync            SyncHook
	BeforeStep           StepHook
	AfterStep            StepHook
	DryRunLockTimeout    time.Duration
	ExecuteHooksInDryRun bool

	RebuildThreshold int
	RebuildBatchSize int
}

func (cfg SQLiteExecConfig) targetSchemas() []string {
	if len(cfg.TargetSchemas) > 0 {
		return cfg.TargetSchemas
	}
	if cfg.TargetSchema != "" {
		return []string{cfg.TargetSchema}
	}
	return []string{"main"}
}

func (cfg SQLiteExecConfig) primarySchema() string {
	return cfg.targetSchemas()[0]
}

func (cfg SQLiteExecConfig) needsAttach() bool {
	return len(cfg.SQLiteAttach) > 0
}

// SyncSQLite synchronizes SQLite in a single transaction with foreign keys handling.
func SyncSQLite(ctx context.Context, db *sql.DB, cfg SQLiteExecConfig) (err error) {
	start := time.Now()
	var p *plan.Plan
	defer func() {
		if cfg.Metrics != nil {
			status := "applied"
			if err != nil {
				status = "failed"
			} else if p == nil || len(p.Steps) == 0 {
				status = "noop"
			}
			cfg.Metrics.RecordMigrationDuration(ctx, time.Since(start), "sqlite", status)
		}
	}()

	logger := cfg.Logger
	schemas := cfg.targetSchemas()
	if logger != nil {
		logger.InfoContext(ctx, "sqlite: starting SQLite schema synchronization", "target_schemas", schemas)
	}

	var histErr error

	run := func(conn *sql.Conn) error {
		latest, err := history.GetLatest(ctx, conn, "sqlite", "")
		if err != nil {
			return fmt.Errorf("sqlite: checking migration history: %w", err)
		}
		if latest != nil && (latest.Status == "partial" || latest.Status == "failed") {
			if !cfg.AllowResumePartial {
				if latest.Error != "" {
					return fmt.Errorf("%w: previous migration ended in %q status (plan %s, step %d): %s; resolve failure with 'grizzle repair' or resume with WithAllowResumePartial / Options.AllowResumePartial",
						plan.ErrPriorFailureBlocked, latest.Status, latest.PlanHash, latest.FailedStep, latest.Error)
				}
				return fmt.Errorf("%w: previous migration ended in %q status (plan %s, step %d); resolve failure with 'grizzle repair' or resume with WithAllowResumePartial / Options.AllowResumePartial",
					plan.ErrPriorFailureBlocked, latest.Status, latest.PlanHash, latest.FailedStep)
			}
			if logger != nil {
				logger.WarnContext(ctx, "sqlite: resuming prior partial/failed migration",
					"prior_status", latest.Status,
					"prior_plan", latest.PlanHash,
					"prior_step", latest.FailedStep,
				)
			}
		}

		p, err = buildSQLitePlan(ctx, conn, cfg)
		if err != nil {
			if logger != nil && errors.Is(err, plan.ErrCompilationFailed) {
				logger.ErrorContext(ctx, "sqlite: shadow compilation failed", "error", err)
			}
			return err
		}

		if cfg.ExpectedHash != "" && p.Hash() != cfg.ExpectedHash {
			if cfg.Metrics != nil {
				cfg.Metrics.RecordDriftDetected(ctx, "sqlite", cfg.primarySchema())
			}
			if logger != nil {
				logger.WarnContext(ctx, "sqlite: plan drift detected post-lock", "expected", cfg.ExpectedHash, "actual", p.Hash())
			}
			return fmt.Errorf("%w: expected hash %q, actual post-lock hash %q", plan.ErrPlanDrift, cfg.ExpectedHash, p.Hash())
		}

		if len(p.Steps) == 0 {
			if logger != nil {
				logger.InfoContext(ctx, "sqlite: schema is already in sync", "duration", time.Since(start))
			}
			recordNoOpSyncIfStale(ctx, conn, "sqlite", cfg.primarySchema(), cfg.SchemaSQL, p, latest, cfg.DryRun, time.Since(start))
			return nil
		}

		if logger != nil {
			logger.InfoContext(ctx, "sqlite: computed migration plan", "steps_count", len(p.Steps))
		}

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

		if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
			if logger != nil {
				logger.WarnContext(ctx, "sqlite: migration blocked by unaccepted critical hazards", "error", err)
			}
			return err
		}

		if cfg.DryRun {
			return nil
		}

		// BeforeSync runs once before any migration steps are executed.
		if err := callBeforeSync(cfg.BeforeSync, ctx, conn); err != nil {
			hookErr := fmt.Errorf("sqlite: before_sync hook: %w", err)
			if logger != nil {
				logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
			}
			return hookErr
		}

		steps := p.Steps
		err = runSQLiteConnWithForeignKeysOff(ctx, conn, func(tx *sql.Tx) error {
			for i, s := range steps {
				stepStart := time.Now()
				sqlToExec := strings.TrimSpace(s.SQL)
				if sqlToExec == "" {
					continue
				}
				if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(steps), IsNonTx: false}); err != nil {
					_ = tx.Rollback()
					hookErr := fmt.Errorf("before_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", i+1, "error", hookErr)
					}
					histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, hookErr, time.Since(start)); rErr != nil && logger != nil {
						logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
					}
					return hookErr
				}
				if err := executeSQLiteStep(ctx, tx, s, cfg); err != nil {
					_ = tx.Rollback()
					if logger != nil {
						logger.ErrorContext(ctx, "sqlite: failed executing step", "step_index", i+1, "sql", sqlToExec, "error", err)
					}
					histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, err, time.Since(start)); rErr != nil && logger != nil {
						logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
					}
					return fmt.Errorf("%w: failed executing [%s]: %w", plan.ErrExecutionFailed, sqlToExec, err)
				}
				if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(steps), IsNonTx: false}); err != nil {
					_ = tx.Rollback()
					hookErr := fmt.Errorf("after_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", i+1, "error", hookErr)
					}
					histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, hookErr, time.Since(start)); rErr != nil && logger != nil {
						logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
					}
					return hookErr
				}
				if logger != nil {
					logger.DebugContext(ctx, "sqlite: executed step", "step_index", i+1, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}
			// Cross-database FKs are not enforced by SQLite; check each attached schema independently.
			return sqliteForeignKeyCheck(ctx, tx, schemas)
		})
		return err
	}

	if cfg.needsAttach() {
		err = withSQLiteAttachedConn(ctx, db, cfg.SQLiteAttach, run)
	} else {
		err = withSQLiteAttachedConn(ctx, db, nil, run)
	}
	if err != nil {
		return err
	}
	if cfg.DryRun || p == nil || len(p.Steps) == 0 {
		return nil
	}

	// Model B: history is advisory — recorded after DDL commit.
	if err := history.RecordPlan(ctx, db, "sqlite", "", p, time.Since(start)); err != nil {
		histErr = fmt.Errorf("%w: %w", plan.ErrHistoryRecord, err)
		if logger != nil {
			logger.ErrorContext(ctx, "sqlite: failed recording history post-commit", "error", err)
		}
	}
	if histErr != nil {
		return histErr
	}

	if cfg.Backfill != nil && cfg.Filters.ExpandContract {
		if err := RunBackfill(ctx, db, cfg.primarySchema(), cfg.Filters.Renames, p.Steps, cfg.Backfill, cfg.Logger); err != nil {
			return err
		}
	}

	if err := callAfterSync(cfg.AfterSync, ctx, db); err != nil {
		hookErr := fmt.Errorf("%w: %w", plan.ErrAfterSyncFailed, err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return hookErr
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: synchronization finished successfully", "steps_applied", len(p.Steps), "total_duration", time.Since(start))
	}

	return nil
}

// PlanDiffSQLite generates the migration plan for SQLite without applying it.
func PlanDiffSQLite(ctx context.Context, db *sql.DB, cfg SQLiteExecConfig) (*plan.Plan, error) {
	var p *plan.Plan
	err := withSQLiteAttachedConn(ctx, db, cfg.SQLiteAttach, func(conn *sql.Conn) error {
		var err error
		p, err = buildSQLitePlan(ctx, conn, cfg)
		return err
	})
	return p, err
}

// ApplySQLite applies a precomputed plan to SQLite in a single transaction with foreign keys handling.
func ApplySQLite(ctx context.Context, db *sql.DB, p *plan.Plan, cfg SQLiteExecConfig) (err error) {
	start := time.Now()
	defer func() {
		if cfg.Metrics != nil && p != nil {
			status := "applied"
			if err != nil {
				status = "failed"
			} else if len(p.Steps) == 0 {
				status = "noop"
			}
			cfg.Metrics.RecordMigrationDuration(ctx, time.Since(start), "sqlite", status)
		}
	}()

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

	logger := cfg.Logger
	if logger != nil {
		logger.InfoContext(ctx, "sqlite: starting SQLite plan application")
	}

	schemas := cfg.targetSchemas()
	if len(p.TargetSchemas) > 0 {
		schemas = p.TargetSchemas
	}

	var histErr error
	err = withSQLiteAttachedConn(ctx, db, cfg.SQLiteAttach, func(conn *sql.Conn) error {
		// BeforeSync runs once before any migration steps are executed.
		if err := callBeforeSync(cfg.BeforeSync, ctx, conn); err != nil {
			hookErr := fmt.Errorf("sqlite: before_sync hook: %w", err)
			if logger != nil {
				logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
			}
			return hookErr
		}

		return runSQLiteConnWithForeignKeysOff(ctx, conn, func(tx *sql.Tx) error {
			// Idempotency: if this plan has already been applied by another process, skip execution.
			if history.IsApplied(ctx, tx, "sqlite", "", p.Hash()) {
				if logger != nil {
					logger.InfoContext(ctx, "sqlite: plan already applied by another process, skipping", "plan_hash", p.Hash())
				}
				return nil
			}

			for i, s := range p.Steps {
				stepStart := time.Now()
				sqlToExec := strings.TrimSpace(s.SQL)
				if sqlToExec == "" {
					continue
				}
				if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(p.Steps), IsNonTx: false}); err != nil {
					_ = tx.Rollback()
					hookErr := fmt.Errorf("before_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", i+1, "error", hookErr)
					}
					histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, hookErr, time.Since(start)); rErr != nil && logger != nil {
						logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
					}
					return hookErr
				}
				if err := executeSQLiteStep(ctx, tx, s, cfg); err != nil {
					_ = tx.Rollback()
					if logger != nil {
						logger.ErrorContext(ctx, "sqlite: failed executing step", "step_index", i+1, "sql", sqlToExec, "error", err)
					}
					histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, err, time.Since(start)); rErr != nil && logger != nil {
						logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
					}
					return fmt.Errorf("%w: failed executing [%s]: %w", plan.ErrExecutionFailed, sqlToExec, err)
				}
				if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(p.Steps), IsNonTx: false}); err != nil {
					_ = tx.Rollback()
					hookErr := fmt.Errorf("after_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", i+1, "error", hookErr)
					}
					histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, hookErr, time.Since(start)); rErr != nil && logger != nil {
						logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
					}
					return hookErr
				}
				if logger != nil {
					logger.DebugContext(ctx, "sqlite: executed step", "step_index", i+1, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}
			return sqliteForeignKeyCheck(ctx, tx, schemas)
		})
	})
	if err != nil {
		return err
	}
	// Model B: history is advisory — recorded after DDL commit.
	if err := history.RecordPlan(ctx, db, "sqlite", "", p, time.Since(start)); err != nil {
		histErr = fmt.Errorf("%w: %w", plan.ErrHistoryRecord, err)
		if logger != nil {
			logger.ErrorContext(ctx, "sqlite: failed recording history post-commit", "error", err)
		}
	}
	if histErr != nil {
		return histErr
	}

	if cfg.Backfill != nil && p.ExpandContract {
		primary := cfg.primarySchema()
		if p.TargetSchema != "" {
			primary = p.TargetSchema
		}
		if err := RunBackfill(ctx, db, primary, p.Renames, p.Steps, cfg.Backfill, cfg.Logger); err != nil {
			return err
		}
	}

	if err := callAfterSync(cfg.AfterSync, ctx, db); err != nil {
		hookErr := fmt.Errorf("%w: %w", plan.ErrAfterSyncFailed, err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return hookErr
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: plan application finished successfully", "steps_applied", len(p.Steps), "total_duration", time.Since(start))
	}

	return nil
}

func executeSQLiteStep(ctx context.Context, tx *sql.Tx, s plan.Step, cfg SQLiteExecConfig) error {
	var span Span
	if cfg.Tracer != nil {
		_, span = cfg.Tracer.Start(ctx, "grizzle.exec_step")
		span.SetAttribute("step.type", string(s.Type))
		span.SetAttribute("step.table", s.Table)
		span.SetAttribute("step.sql", s.SQL)
		span.SetAttribute("step.non_tx", false)
	}
	err := executeSQLiteStepInner(ctx, tx, s, cfg)
	if span != nil {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}
	return err
}

func executeSQLiteStepInner(ctx context.Context, tx *sql.Tx, s plan.Step, cfg SQLiteExecConfig) error {
	if !s.IsTableRebuild {
		sqlToExec := strings.TrimSpace(s.SQL)
		if sqlToExec == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx, sqlToExec)
		return err
	}

	// Table rebuild step: wrap in SAVEPOINT grizzle_rebuild
	if _, err := tx.ExecContext(ctx, "SAVEPOINT grizzle_rebuild;"); err != nil {
		return fmt.Errorf("creating savepoint: %w", err)
	}

	// Clean up any stale temporary tables leftover from an interrupted prior rebuild attempt.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quoteSQLiteTable(s.Schema, "_grizzle_new_"+s.Table))); err != nil {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
		_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
		return fmt.Errorf("cleaning stale temp table _grizzle_new_%s: %w", s.Table, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s;", quoteSQLiteTable(s.Schema, "_grizzle_tmp_"+s.Table))); err != nil {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
		_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
		return fmt.Errorf("cleaning stale temp table _grizzle_tmp_%s: %w", s.Table, err)
	}

	threshold := cfg.RebuildThreshold
	if threshold == 0 {
		threshold = 100000
	}
	batchSize := cfg.RebuildBatchSize
	if batchSize <= 0 {
		batchSize = 10000
	}

	tblRef := quoteSQLiteTable(s.Schema, s.Table)
	tempTable := "_grizzle_new_" + s.Table
	tempRef := quoteSQLiteTable(s.Schema, tempTable)

	var rowCount int64
	_ = tx.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s;", tblRef)).Scan(&rowCount)

	// Rebuild SQL may quote temp as schema.temp or bare "temp".
	copyDataPrefix := fmt.Sprintf("INSERT INTO %s", tempRef)
	idx := strings.Index(s.SQL, copyDataPrefix)
	if idx == -1 {
		copyDataPrefix = fmt.Sprintf("INSERT INTO %q", tempTable)
		idx = strings.Index(s.SQL, copyDataPrefix)
	}

	// WITHOUT ROWID tables have no usable rowid for keyset batching.
	hasRowID := true
	var probeDDL string
	catalog := "sqlite_master"
	if s.Schema != "" && s.Schema != "main" {
		catalog = fmt.Sprintf("%q.sqlite_schema", s.Schema)
	}
	if qErr := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT sql FROM %s WHERE type='table' AND name=?`, catalog), s.Table).Scan(&probeDDL); qErr == nil {
		if strings.Contains(strings.ToUpper(probeDDL), "WITHOUT ROWID") {
			hasRowID = false
			if cfg.Logger != nil {
				cfg.Logger.DebugContext(ctx, "sqlite: skipping rowid keyset copy for WITHOUT ROWID table", "table", s.Table)
			}
		}
	}

	needsChunked := threshold > 0 && rowCount > int64(threshold) && idx != -1 && hasRowID

	if needsChunked {
		beforeCopy := strings.TrimSpace(s.SQL[:idx])
		rest := s.SQL[idx:]
		semiIdx := strings.Index(rest, ";")
		if semiIdx == -1 {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
			_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
			return fmt.Errorf("malformed copy statement in rebuild SQL")
		}
		afterCopy := strings.TrimSpace(rest[semiIdx+1:])

		if beforeCopy != "" {
			if _, err := tx.ExecContext(ctx, beforeCopy); err != nil {
				_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
				_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
				return fmt.Errorf("executing create temp table: %w", err)
			}
		}

		if len(s.Columns) == 0 {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
			_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
			return fmt.Errorf("sqlite: table %q rebuild step requires chunked copying but has empty Columns list", s.Table)
		}

		quotedCols := make([]string, len(s.Columns))
		for i, col := range s.Columns {
			quotedCols[i] = quoteIdent(col)
		}
		colList := strings.Join(quotedCols, ", ")
		if err := copyDataKeysetChunks(ctx, tx, tempRef, colList, tblRef, batchSize, cfg.Logger); err != nil {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
			_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
			return err
		}

		if afterCopy != "" {
			if _, err := tx.ExecContext(ctx, afterCopy); err != nil {
				_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
				_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
				return fmt.Errorf("executing post-copy rebuild statements: %w", err)
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
			_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
			return fmt.Errorf("executing rebuild SQL: %w", err)
		}
	}

	// Validate foreign keys under the savepoint (per-schema; cross-DB FKs are not enforced).
	fkPragma := "PRAGMA foreign_key_check;"
	if s.Schema != "" && s.Schema != "main" {
		fkPragma = fmt.Sprintf("PRAGMA %q.foreign_key_check;", s.Schema)
	}
	fkRows, err := tx.QueryContext(ctx, fkPragma)
	if err != nil {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
		_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
		return fmt.Errorf("foreign key check query failed: %w", err)
	}
	var fkViolations []string
	for fkRows.Next() {
		var vTbl, vParent string
		var vRowID, vFKID int64
		if err := fkRows.Scan(&vTbl, &vRowID, &vParent, &vFKID); err == nil {
			fkViolations = append(fkViolations, fmt.Sprintf("table %q row %d -> %q", vTbl, vRowID, vParent))
		}
	}
	_ = fkRows.Close()

	if len(fkViolations) > 0 {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
		_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
		return fmt.Errorf("foreign key constraint violation detected after rebuilding table %q: %s", s.Table, strings.Join(fkViolations, ", "))
	}

	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;"); err != nil {
		return fmt.Errorf("releasing savepoint grizzle_rebuild: %w", err)
	}

	return nil
}

// quoteSQLiteTable returns a quoted table reference, schema-qualified when needed.
func quoteSQLiteTable(schemaName, tableName string) string {
	if schemaName == "" || schemaName == "main" {
		return quoteIdent(tableName)
	}
	return quoteIdent(schemaName) + "." + quoteIdent(tableName)
}

// quoteIdent quotes a SQL identifier with double quotes, escaping embedded quotes.
func quoteIdent(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && len(s) >= 2 {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// copyDataKeysetChunks copies rows in batches. tempRef and liveRef must already
// be SQL-quoted table references (optionally schema-qualified).
func copyDataKeysetChunks(ctx context.Context, tx *sql.Tx, tempRef, colList, liveRef string, batchSize int, logger *slog.Logger) error {
	var lastRowID int64
	var hasStarted bool
	var batchNum int

	for {
		batchNum++
		var query string
		var args []any
		if !hasStarted {
			query = fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s ORDER BY rowid ASC LIMIT ?;", tempRef, colList, colList, liveRef)
			args = []any{batchSize}
		} else {
			query = fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s WHERE rowid > ? ORDER BY rowid ASC LIMIT ?;", tempRef, colList, colList, liveRef)
			args = []any{lastRowID, batchSize}
		}

		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("sqlite: chunked keyset copy failed: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("sqlite: failed checking rows affected in keyset copy: %w", err)
		}
		if affected == 0 {
			break
		}
		if logger != nil {
			logger.DebugContext(ctx, "sqlite: copied keyset batch", "table", liveRef, "batch", batchNum, "rows", affected)
		}

		var maxRowID int64
		var maxQuery string
		var maxArgs []any
		if !hasStarted {
			maxQuery = fmt.Sprintf("SELECT MAX(rowid) FROM (SELECT rowid FROM %s ORDER BY rowid ASC LIMIT ?);", liveRef)
			maxArgs = []any{batchSize}
		} else {
			maxQuery = fmt.Sprintf("SELECT MAX(rowid) FROM (SELECT rowid FROM %s WHERE rowid > ? ORDER BY rowid ASC LIMIT ?);", liveRef)
			maxArgs = []any{lastRowID, batchSize}
		}

		if err := tx.QueryRowContext(ctx, maxQuery, maxArgs...).Scan(&maxRowID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			return fmt.Errorf("sqlite: failed querying max rowid in keyset copy: %w", err)
		}
		lastRowID = maxRowID
		hasStarted = true
	}
	return nil
}
