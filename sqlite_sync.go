package grizzle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func syncSQLite(ctx context.Context, db *sql.DB, opts Options) error {
	start := time.Now()
	logger := opts.Logger
	if logger != nil {
		logger.InfoContext(ctx, "grizzle: starting SQLite schema synchronization")
	}

	// 1. Open isolated in-memory shadow database to validate and compile schema.sql
	shadowDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return fmt.Errorf("sqlite: failed to open in-memory shadow db: %w", err)
	}
	defer func() { _ = shadowDB.Close() }()

	shadowStart := time.Now()
	if _, err := shadowDB.ExecContext(ctx, opts.SchemaSQL); err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "sqlite: shadow compilation failed", "error", err)
		}
		return fmt.Errorf("%w: %v", ErrCompilationFailed, err)
	}
	if logger != nil {
		logger.DebugContext(ctx, "sqlite: shadow compilation succeeded", "duration", time.Since(shadowStart))
	}

	// 2. Introspect desired schema from shadowDB
	desired, err := inspectSQLiteSchema(ctx, shadowDB)
	if err != nil {
		return err
	}

	// 3. Introspect live schema from live db
	live, err := inspectSQLiteSchema(ctx, db)
	if err != nil {
		return err
	}

	// 4. Diff schemas
	steps := diffSQLiteSchemas(live, desired)
	policy := resolveDropPolicy(opts)

	if len(steps) == 0 {
		if logger != nil {
			logger.InfoContext(ctx, "sqlite: schema is already in sync", "duration", time.Since(start))
		}
		return nil
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: computed migration plan", "steps_count", len(steps))
	}

	// 5. Enforce safety policy
	var violations []Step
	for _, s := range steps {
		if !policy.IsAllowed(s) {
			violations = append(violations, s)
		}
	}
	if len(violations) > 0 {
		if logger != nil {
			logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "violations_count", len(violations))
		}
		return &DestructiveViolationError{Violations: violations}
	}

	// 6. Execute inside a transaction with foreign keys handling
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Disable foreign keys during table recreation as per 12-step procedure
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
			return fmt.Errorf("%w: failed executing [%s]: %v", ErrExecutionFailed, sqlToExec, err)
		}
		if logger != nil {
			logger.DebugContext(ctx, "sqlite: executed step", "step_index", i+1, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
		}
	}

	// Re-enable and check foreign keys
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

func planDiffSQLite(ctx context.Context, db *sql.DB, opts Options) (*Plan, error) {
	shadowDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("sqlite: failed to open in-memory shadow db: %w", err)
	}
	defer func() { _ = shadowDB.Close() }()

	if _, err := shadowDB.ExecContext(ctx, opts.SchemaSQL); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCompilationFailed, err)
	}

	desired, err := inspectSQLiteSchema(ctx, shadowDB)
	if err != nil {
		return nil, err
	}

	live, err := inspectSQLiteSchema(ctx, db)
	if err != nil {
		return nil, err
	}

	steps := diffSQLiteSchemas(live, desired)
	policy := resolveDropPolicy(opts)

	return &Plan{
		TargetSchema: "main",
		Steps:        steps,
		Policy:       policy,
	}, nil
}
