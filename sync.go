package grizzle

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func prepareOptions(opts *Options) error {
	if strings.TrimSpace(opts.SchemaSQL) == "" {
		return ErrEmptySchema
	}
	opts.TargetSchema = cmp.Or(opts.TargetSchema, "public")
	opts.ShadowSchema = cmp.Or(opts.ShadowSchema, "_grizzle_shadow")
	if opts.LockID == 0 {
		opts.LockID = generateLockID(opts.TargetSchema)
	}
	return nil
}

// Sync synchronizes the target database schema to match the desired state in opts.SchemaSQL.
// It executes in a single transaction protected by a PostgreSQL advisory lock.
func Sync(ctx context.Context, db *sql.DB, opts Options) error {
	if err := prepareOptions(&opts); err != nil {
		return err
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
	if err := prepareOptions(&opts); err != nil {
		return nil, err
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
