package exec

import (
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yourorg/grizzle/internal/plan"
)

type sqlStateCoder interface {
	SQLState() string
}

// IsLockTimeout checks whether an error corresponds to PostgreSQL lock_timeout (SQLSTATE 55P03).
func IsLockTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, plan.ErrLockTimeout) {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return true
	}

	var coder sqlStateCoder
	if errors.As(err, &coder) && coder.SQLState() == "55P03" {
		return true
	}

	msg := err.Error()
	return strings.Contains(msg, "55P03") ||
		strings.Contains(msg, "lock_not_available") ||
		strings.Contains(msg, "canceling statement due to lock timeout")
}

// ComputeBackoff calculates an exponential backoff with jitter for a given retry attempt.
func ComputeBackoff(attempt int, randFn func() float64) time.Duration {
	base := 50 * time.Millisecond
	maxBackoff := 2 * time.Second

	factor := 1 << min(attempt, 6)
	backoff := base * time.Duration(factor)
	if backoff > maxBackoff {
		backoff = maxBackoff
	}

	var r float64
	if randFn != nil {
		r = randFn()
	} else {
		r = rand.Float64()
	}

	// Full jitter: between 50% and 100% of backoff
	jitter := 0.5 + (0.5 * r)
	return time.Duration(float64(backoff) * jitter)
}
