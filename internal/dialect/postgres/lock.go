package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/yourorg/grizzle/internal/dialect"
	"github.com/yourorg/grizzle/internal/plan"
)

// GenerateLockID produces a deterministic 64-bit integer hash from a schema identifier.
func GenerateLockID(schema string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("grizzle:schema_lock:" + schema))
	return int64(h.Sum64())
}

// AcquireAdvisoryLock acquires a PostgreSQL transaction-level exclusive advisory lock.
// This lock is automatically released by PostgreSQL when the transaction commits or aborts.
func AcquireAdvisoryLock(ctx context.Context, dbtx dialect.DBTX, lockID int64) error {
	var dummy int
	err := dbtx.QueryRowContext(ctx, "SELECT 1 FROM pg_advisory_xact_lock($1);", lockID).Scan(&dummy)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to acquire pg_advisory_xact_lock: %w", err)
	}
	return nil
}

// AcquireSessionAdvisoryLock acquires a PostgreSQL session-level exclusive advisory lock on a dedicated connection.
// It uses pg_try_advisory_lock in a non-blocking loop to avoid holding open server-side lock wait queues
// that would cause PostgreSQL deadlock detection against concurrent non-transactional DDL such as CREATE INDEX CONCURRENTLY.
func AcquireSessionAdvisoryLock(ctx context.Context, dbtx dialect.DBTX, lockID int64) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		var acquired bool
		err := dbtx.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1);", lockID).Scan(&acquired)
		if err != nil {
			return fmt.Errorf("failed checking pg_try_advisory_lock: %w", err)
		}
		if acquired {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", plan.ErrLockTimeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// ReleaseSessionAdvisoryLock releases a PostgreSQL session-level exclusive advisory lock on a dedicated connection.
func ReleaseSessionAdvisoryLock(ctx context.Context, dbtx dialect.DBTX, lockID int64) error {
	var dummy int
	err := dbtx.QueryRowContext(ctx, "SELECT 1 FROM pg_advisory_unlock($1);", lockID).Scan(&dummy)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to release pg_advisory_unlock: %w", err)
	}
	return nil
}
