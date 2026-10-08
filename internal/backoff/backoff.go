package backoff

import (
	"math/rand/v2"
	"time"
)

// Compute calculates an exponential backoff with jitter for a given retry attempt.
func Compute(attempt int, randFn func() float64) time.Duration {
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
