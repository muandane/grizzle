package grizzle

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
)

// generateLockID produces a deterministic 64-bit integer hash from a schema identifier.
func generateLockID(schema string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("grizzle:schema_lock:" + schema))
	return int64(h.Sum64())
}

// acquireAdvisoryLock acquires a PostgreSQL transaction-level exclusive advisory lock.
// This lock is automatically released by PostgreSQL when the transaction commits or aborts.
// Concurrent callers will block here until the holding transaction finishes.
func acquireAdvisoryLock(ctx context.Context, tx *sql.Tx, lockID int64) error {
	var dummy int
	// pg_advisory_xact_lock returns void, so we select 1 to scan into a dummy variable.
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM pg_advisory_xact_lock($1);", lockID).Scan(&dummy)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("%w: %v", ErrLockAcquisition, err)
	}
	return nil
}
