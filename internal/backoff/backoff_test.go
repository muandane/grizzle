package backoff

import (
	"testing"
	"time"
)

// deterministic rand funcs mirroring exec/retry_test.go expectations.
func minRand() float64 { return 0.0 }
func maxRand() float64 { return 1.0 }

func TestCompute_GrowthAndJitterBounds(t *testing.T) {
	b1Min := Compute(1, minRand)
	b1Max := Compute(1, maxRand)
	if b1Min != 50*time.Millisecond {
		t.Errorf("attempt 1 min backoff = %v, want 50ms", b1Min)
	}
	if b1Max != 100*time.Millisecond {
		t.Errorf("attempt 1 max backoff = %v, want 100ms", b1Max)
	}

	b2Min := Compute(2, minRand)
	b2Max := Compute(2, maxRand)
	if b2Min != 100*time.Millisecond {
		t.Errorf("attempt 2 min backoff = %v, want 100ms", b2Min)
	}
	if b2Max != 200*time.Millisecond {
		t.Errorf("attempt 2 max backoff = %v, want 200ms", b2Max)
	}

	if got := Compute(10, maxRand); got != DefaultMax {
		t.Errorf("attempt 10 capped backoff = %v, want %v", got, DefaultMax)
	}
}

func TestComputeWithMax_CustomCap(t *testing.T) {
	const cap250 = 250 * time.Millisecond

	// Grows exponentially until the custom cap.
	if got := ComputeWithMax(1, maxRand, cap250); got != 100*time.Millisecond {
		t.Errorf("attempt 1 = %v, want 100ms", got)
	}
	if got := ComputeWithMax(2, maxRand, cap250); got != 200*time.Millisecond {
		t.Errorf("attempt 2 = %v, want 200ms", got)
	}
	if got := ComputeWithMax(3, maxRand, cap250); got != cap250 {
		t.Errorf("attempt 3 = %v, want capped %v", got, cap250)
	}
	if got := ComputeWithMax(10, minRand, cap250); got != cap250/2 {
		t.Errorf("attempt 10 min = %v, want %v (50%% of cap)", got, cap250/2)
	}
}
