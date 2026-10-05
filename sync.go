package grizzle

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func detectDialect(ctx context.Context, db *sql.DB) (Dialect, error) {
	if db == nil {
		return "", fmt.Errorf("grizzle: database connection is nil")
	}

	// 1. Inspect driver type if available
	if drv := db.Driver(); drv != nil {
		drvName := strings.ToLower(fmt.Sprintf("%T", drv))
		switch {
		case strings.Contains(drvName, "sqlite"):
			return DialectSQLite, nil
		case strings.Contains(drvName, "pgx"), strings.Contains(drvName, "pq"), strings.Contains(drvName, "postgres"):
			return DialectPostgres, nil
		}
	}

	// 2. Query probing fallback
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
			opts.LockID = generateLockID(opts.TargetSchema)
		}
	default:
		return fmt.Errorf("grizzle: unsupported dialect %q", opts.Dialect)
	}
	return nil
}

// Sync synchronizes the target database schema to match the desired state in opts.SchemaSQL.
// For PostgreSQL, it executes in a single transaction protected by an advisory lock.
// For SQLite, it executes in a transaction with foreign keys handling.
func Sync(ctx context.Context, db *sql.DB, opts Options) error {
	if err := prepareOptions(ctx, db, &opts); err != nil {
		return err
	}

	if opts.Dialect == DialectSQLite {
		return syncSQLite(ctx, db, opts)
	}

	start := time.Now()
	logger := opts.Logger
	if logger != nil {
		logger.InfoContext(ctx, "grizzle: starting schema synchronization", "target_schema", opts.TargetSchema)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("grizzle: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Acquire transaction advisory lock
	if err := acquireAdvisoryLock(ctx, tx, opts.LockID); err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: lock acquisition failed", "lock_id", opts.LockID, "error", err)
		}
		return err
	}
	if logger != nil {
		logger.DebugContext(ctx, "grizzle: acquired advisory lock", "lock_id", opts.LockID)
	}

	// 2. Setup shadow schema and ensure it is cleaned up on exit
	if err := setupShadowSchema(ctx, tx, opts.ShadowSchema); err != nil {
		return err
	}
	defer func() { _ = dropShadowSchema(context.Background(), tx, opts.ShadowSchema) }()

	// 3. Compile user's schema.sql inside shadow schema
	shadowStart := time.Now()
	if err := runShadowDDL(ctx, tx, opts.ShadowSchema, opts.TargetSchema, opts.SchemaSQL); err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: shadow compilation failed", "error", err)
		}
		return err
	}
	if logger != nil {
		logger.DebugContext(ctx, "grizzle: shadow compilation succeeded", "duration", time.Since(shadowStart))
	}

	// 4. Introspect both live and desired schemas
	live, err := inspectSchema(ctx, tx, opts.TargetSchema)
	if err != nil {
		return err
	}

	desired, err := inspectSchema(ctx, tx, opts.ShadowSchema)
	if err != nil {
		return err
	}

	// 5. Diff schemas
	steps := diffSchemas(live, desired, opts.TargetSchema, opts.ShadowSchema)
	policy := resolveDropPolicy(opts)

	if len(steps) == 0 {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: schema is already in sync", "duration", time.Since(start))
		}
		_ = dropShadowSchema(ctx, tx, opts.ShadowSchema)
		return tx.Commit()
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: computed migration plan", "steps_count", len(steps))
	}

	// 6. Enforce fine-grained safety policy
	var violations []Step
	for _, s := range steps {
		if !policy.IsAllowed(s) {
			violations = append(violations, s)
		}
	}
	if len(violations) > 0 {
		if logger != nil {
			logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "violations_count", len(violations))
		}
		return &DestructiveViolationError{Violations: violations}
	}

	// 7. Apply DDL statements to live schema
	for i, s := range steps {
		stepStart := time.Now()
		if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
			if logger != nil {
				logger.ErrorContext(ctx, "grizzle: failed executing step", "step_index", i+1, "sql", s.SQL, "error", err)
			}
			return fmt.Errorf("%w: failed executing [%s]: %v", ErrExecutionFailed, s.SQL, err)
		}
		if logger != nil {
			logger.DebugContext(ctx, "grizzle: executed step", "step_index", i+1, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
		}
	}

	// 8. Clean up shadow schema
	if err := dropShadowSchema(ctx, tx, opts.ShadowSchema); err != nil {
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

// PlanDiff inspects the live database and computes the planned migration steps without applying them.
func PlanDiff(ctx context.Context, db *sql.DB, opts Options) (*Plan, error) {
	if err := prepareOptions(ctx, db, &opts); err != nil {
		return nil, err
	}

	if opts.Dialect == DialectSQLite {
		return planDiffSQLite(ctx, db, opts)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Acquire advisory lock
	if err := acquireAdvisoryLock(ctx, tx, opts.LockID); err != nil {
		return nil, err
	}

	// 2. Setup shadow schema
	if err := setupShadowSchema(ctx, tx, opts.ShadowSchema); err != nil {
		return nil, err
	}
	defer func() { _ = dropShadowSchema(context.Background(), tx, opts.ShadowSchema) }()

	// 3. Compile schema.sql in shadow
	if err := runShadowDDL(ctx, tx, opts.ShadowSchema, opts.TargetSchema, opts.SchemaSQL); err != nil {
		return nil, err
	}

	// 4. Introspect
	live, err := inspectSchema(ctx, tx, opts.TargetSchema)
	if err != nil {
		return nil, err
	}

	desired, err := inspectSchema(ctx, tx, opts.ShadowSchema)
	if err != nil {
		return nil, err
	}

	// 5. Diff
	steps := diffSchemas(live, desired, opts.TargetSchema, opts.ShadowSchema)
	policy := resolveDropPolicy(opts)

	return &Plan{
		TargetSchema: opts.TargetSchema,
		Steps:        steps,
		Policy:       policy,
	}, nil
}
