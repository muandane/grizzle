package exec

import (
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/muandane/grizzle/internal/plan"
)

type sqlStateCoder interface {
	error
	SQLState() string
}

// retryableSQLStates lists the PostgreSQL SQLSTATEs that are safe to retry
// before any migration step has committed:
//
//   - 55P03 lock_not_available: lock_timeout expired while acquiring an
//     advisory lock or a table lock for DDL.
//   - 40P01 deadlock_detected: a transactional DDL group can deadlock against
//     concurrent application transactions that lock the same tables in a
//     different order. PostgreSQL aborts one victim; when the victim is the
//     migration transaction, the whole group rolls back with nothing
//     committed, so a full retry is safe.
//
// 40001 (serialization_failure) is deliberately NOT retryable: Grizzle runs
// under the default READ COMMITTED isolation, where serialization failures
// cannot occur, and the retry loop only retries lock-classified errors.
var retryableSQLStates = map[string]bool{
	"55P03": true,
	"40P01": true,
}

// IsRetryable reports whether an error is transient and safe to retry while
// no migration step has committed yet (see SyncPostgres and ApplyPostgres).
//
// Classification is SQLSTATE-based only: errors are inspected through
// *pgconn.PgError (pgx driver) or the database/sql SQLState() error interface
// (any other driver, e.g. lib/pq). Message text is never matched, because
// error text is user-influenced (identifiers, constraint names) and would
// misclassify ordinary failures as retryable.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Sentinel produced by our own advisory-lock acquisition budget.
	if errors.Is(err, plan.ErrLockTimeout) {
		return true
	}

	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return retryableSQLStates[pgErr.Code]
	}

	if coder, ok := errors.AsType[sqlStateCoder](err); ok {
		return retryableSQLStates[coder.SQLState()]
	}

	return false
}

// ComputeBackoff calculates an exponential backoff with jitter for a given retry attempt.
func ComputeBackoff(attempt int, randFn func() float64) time.Duration {
	base := 50 * time.Millisecond
	maxBackoff := 2 * time.Second

	factor := 1 << min(attempt, 6)
	backoff := min(base*time.Duration(factor), maxBackoff)

	var r float64
	if randFn != nil {
		r = randFn()
	} else {
		r = rand.Float64() //nolint:gosec // G404: weak random is sufficient for retry backoff jitter
	}

	// Full jitter: between 50% and 100% of backoff
	jitter := 0.5 + (0.5 * r)
	return time.Duration(float64(backoff) * jitter)
}
