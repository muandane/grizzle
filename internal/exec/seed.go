package exec

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/history"
	"github.com/muandane/grizzle/internal/plan"
)

// SeedExecConfig configures an idempotent seed execution.
type SeedExecConfig struct {
	// TargetSchemas lists the PostgreSQL schemas the seed runs against
	// (defaults to ["public"]). Ignored on SQLite.
	TargetSchemas []string

	// LockID is an optional explicit 64-bit integer advisory lock used to
	// serialize concurrent seed runs on PostgreSQL.
	LockID int64

	// LockNamespace is the application namespace string used to derive
	// per-schema advisory locks (defaults to "grizzle").
	LockNamespace string

	// LockTimeout bounds how long the seed waits for the advisory lock
	// (defaults to DefaultLockTimeout). PostgreSQL only.
	LockTimeout time.Duration

	// StatementTimeout bounds the seed execution time. PostgreSQL only.
	StatementTimeout time.Duration

	// Force re-runs the seed even when the same seed hash is already
	// recorded as applied.
	Force bool

	// Logger accepts a structured logger for seed events.
	Logger *slog.Logger
}

// SeedHash returns the deterministic sha256 identity of a seed script.
// Leading and trailing whitespace is ignored so reformatted-but-identical
// seeds do not re-run.
func SeedHash(seedSQL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(seedSQL)))
	return hex.EncodeToString(sum[:])
}

// SeedPostgres executes idempotent seed SQL on PostgreSQL. The seed runs in a
// single transaction under a session-level advisory lock; either the whole
// seed commits with an 'applied' history record keyed by its seed hash, or
// nothing persists. If the same seed hash was already applied and Force is
// false, the seed is skipped. Forced re-runs execute the script again, so
// seed scripts should tolerate re-execution (IF NOT EXISTS, ON CONFLICT...).
func SeedPostgres(ctx context.Context, db *sql.DB, cfg SeedExecConfig, seedSQL string) error {
	start := time.Now()
	logger := cfg.Logger

	primarySchema := "public"
	if len(cfg.TargetSchemas) > 0 && cfg.TargetSchemas[0] != "" {
		primarySchema = cfg.TargetSchemas[0]
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("grizzle: failed to acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	_ = ApplySessionTimeouts(ctx, conn, cfg.LockTimeout, cfg.StatementTimeout)

	// Serialize concurrent seed runs on the same schema.
	lockTimeout := cfg.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = DefaultLockTimeout
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, lockTimeout)
	var acquiredSchemas []string
	if cfg.LockID != 0 {
		err = postgres.AcquireSessionAdvisoryLock(lockCtx, conn, cfg.LockID)
	} else {
		lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
		acquiredSchemas, err = postgres.AcquireSchemaLocks(lockCtx, conn, lockNs, cfg.targetSchemasOr(primarySchema))
	}
	cancelLock()
	if err != nil {
		return fmt.Errorf("%w: %w", plan.ErrLockTimeout, err)
	}
	defer func() {
		if cfg.LockID != 0 {
			_ = postgres.ReleaseSessionAdvisoryLock(context.Background(), conn, cfg.LockID)
		} else {
			lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
			_ = postgres.ReleaseSchemaLocks(context.Background(), conn, lockNs, acquiredSchemas)
		}
	}()

	seedHash := SeedHash(seedSQL)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("grizzle: failed to begin seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if !cfg.Force {
		if err := history.EnsureTable(ctx, tx, "postgres", primarySchema); err != nil {
			return err
		}
		if history.IsSeedApplied(ctx, tx, "postgres", primarySchema, seedHash) {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("grizzle: failed to commit seed skip: %w", err)
			}
			if logger != nil {
				logger.InfoContext(ctx, "grizzle: seed already applied, skipping", "seed_hash", seedHash)
			}
			return nil
		}
	}

	if _, err := tx.ExecContext(ctx, seedSQL); err != nil {
		return fmt.Errorf("%w: %w", plan.ErrSeedFailed, err)
	}
	if err := history.RecordSeed(ctx, tx, "postgres", primarySchema, seedHash, time.Since(start)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("grizzle: failed to commit seed: %w", err)
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: seed applied", "seed_hash", seedHash, "duration", time.Since(start))
	}
	return nil
}

// SeedSQLite executes idempotent seed SQL on SQLite inside a single
// transaction that either fully commits with an 'applied' history record
// keyed by its seed hash, or leaves the database untouched. If the same seed
// hash was already applied and Force is false, the seed is skipped.
func SeedSQLite(ctx context.Context, db *sql.DB, cfg SeedExecConfig, seedSQL string) error {
	start := time.Now()
	logger := cfg.Logger

	seedHash := SeedHash(seedSQL)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: failed to begin seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if !cfg.Force {
		if err := history.EnsureTable(ctx, tx, "sqlite", ""); err != nil {
			return err
		}
		if history.IsSeedApplied(ctx, tx, "sqlite", "", seedHash) {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("sqlite: failed to commit seed skip: %w", err)
			}
			if logger != nil {
				logger.InfoContext(ctx, "sqlite: seed already applied, skipping", "seed_hash", seedHash)
			}
			return nil
		}
	}

	if _, err := tx.ExecContext(ctx, seedSQL); err != nil {
		return fmt.Errorf("%w: %w", plan.ErrSeedFailed, err)
	}
	if err := history.RecordSeed(ctx, tx, "sqlite", "", seedHash, time.Since(start)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: failed to commit seed: %w", err)
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: seed applied", "seed_hash", seedHash, "duration", time.Since(start))
	}
	return nil
}

// targetSchemasOr returns the configured target schemas or a single-schema
// fallback.
func (c SeedExecConfig) targetSchemasOr(fallback string) []string {
	if len(c.TargetSchemas) > 0 {
		return c.TargetSchemas
	}
	return []string{fallback}
}
