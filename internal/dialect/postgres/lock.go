package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/yourorg/grizzle/internal/dialect"
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
// It uses pg_try_advisory_lock with non-blocking retries to avoid holding waiting transactions that would
// deadlock against concurrent non-transactional operations such as CREATE INDEX CONCURRENTLY.
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
			return fmt.Errorf("timeout waiting for pg_advisory_lock: %w", ctx.Err())
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
