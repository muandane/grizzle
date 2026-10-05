package grizzle

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"strings"
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

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("grizzle: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Acquire transaction advisory lock
	if err := acquireAdvisoryLock(ctx, tx, opts.LockID); err != nil {
		return err
	}

	// 2. Setup shadow schema and ensure it is cleaned up on exit
	if err := setupShadowSchema(ctx, tx, opts.ShadowSchema); err != nil {
		return err
	}
	defer func() { _ = dropShadowSchema(context.Background(), tx, opts.ShadowSchema) }()

	// 3. Compile user's schema.sql inside shadow schema
	if err := runShadowDDL(ctx, tx, opts.ShadowSchema, opts.TargetSchema, opts.SchemaSQL); err != nil {
		return err
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
	steps := diffSchemas(live, desired, opts.TargetSchema)
	if len(steps) == 0 {
		// Already in sync!
		_ = dropShadowSchema(ctx, tx, opts.ShadowSchema)
		return tx.Commit()
	}

	// 6. Enforce destructive drop safety
	if !opts.AllowDrop {
		for _, s := range steps {
			if s.Destructive {
				return fmt.Errorf("%w: operation [%s on %s] is destructive", ErrDestructiveBlocked, s.Type, s.Table)
			}
		}
	}

	// 7. Apply DDL statements to live schema
	for _, s := range steps {
		if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
			return fmt.Errorf("%w: failed executing [%s]: %v", ErrExecutionFailed, s.SQL, err)
		}
	}

	// 8. Clean up shadow schema
	if err := dropShadowSchema(ctx, tx, opts.ShadowSchema); err != nil {
		return err
	}

	return tx.Commit()
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
	steps := diffSchemas(live, desired, opts.TargetSchema)

	return &Plan{
		TargetSchema: opts.TargetSchema,
		Steps:        steps,
	}, nil
}
