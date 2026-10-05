package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"

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
