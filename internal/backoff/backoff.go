package backoff

import (
	"math/rand/v2"
	"time"
)

const (
	// base is the initial backoff duration.
	base = 50 * time.Millisecond

	// DefaultMax is the default backoff ceiling for retry campaigns.
	DefaultMax = 2 * time.Second
)

// Compute calculates an exponential backoff with jitter for a given retry attempt,
// capped at [DefaultMax].
func Compute(attempt int, randFn func() float64) time.Duration {
	return ComputeWithMax(attempt, randFn, DefaultMax)
}

// ComputeWithMax calculates an exponential backoff with jitter capped at max.
// Callers that need prompt reaction after a wait ends (e.g. advisory-lock
// polling) pass a smaller cap so they never sleep past the release moment.
func ComputeWithMax(attempt int, randFn func() float64, max time.Duration) time.Duration {
	factor := 1 << min(attempt, 6)
	backoff := min(base*time.Duration(factor), max)

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
