package exec_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
)

type customSQLError struct {
	state string
	msg   string
}

func (e *customSQLError) Error() string    { return e.msg }
func (e *customSQLError) SQLState() string { return e.state }

func TestRetry_IsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "ErrLockTimeout sentinel",
			err:  plan.ErrLockTimeout,
			want: true,
		},
		{
			name: "wrapped ErrLockTimeout",
			err:  fmt.Errorf("failed: %w", plan.ErrLockTimeout),
			want: true,
		},
		{
			name: "pgx 55P03 PgError",
			err:  &pgconn.PgError{Code: "55P03", Message: "lock not available"},
			want: true,
		},
		{
			name: "pgx 40P01 deadlock PgError",
			err:  &pgconn.PgError{Code: "40P01", Message: "deadlock detected"},
			want: true,
		},
		{
			name: "pgx 40001 serialization_failure is NOT retryable (READ COMMITTED)",
			err:  &pgconn.PgError{Code: "40001", Message: "could not serialize access"},
			want: false,
		},
		{
			name: "interface SQLState 55P03",
			err:  &customSQLError{state: "55P03", msg: "canceling statement due to lock timeout"},
			want: true,
		},
		{
			name: "interface SQLState 40P01",
			err:  &customSQLError{state: "40P01", msg: "deadlock detected"},
			want: true,
		},
		{
			name: "wrapped pgx 55P03 PgError",
			err:  fmt.Errorf("executing step: %w", &pgconn.PgError{Code: "55P03", Message: "lock not available"}),
			want: true,
		},
		{
			name: "message contains 55P03 but no SQLSTATE interface",
			err:  errors.New("ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)"),
			want: false,
		},
		{
			name: "message contains lock_not_available text only",
			err:  errors.New("tuple lock: lock_not_available somewhere in the text"),
			want: false,
		},
		{
			name: "message contains lock timeout text only",
			err:  errors.New("canceling statement due to lock timeout"),
			want: false,
		},
		{
			name: "identifier mentioning 55P03 is not retryable",
			err:  &pgconn.PgError{Code: "42P01", Message: "relation check_55P03_does_not_exist does not exist"},
			want: false,
		},
		{
			name: "unrelated postgres error",
			err:  &pgconn.PgError{Code: "42P01", Message: "relation does not exist"},
			want: false,
		},
		{
			name: "context deadline exceeded",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "generic error",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := exec.IsRetryable(tt.err)
			if got != tt.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetry_ComputeBackoff(t *testing.T) {
	// Deterministic random func returning fixed 0.0 (minimum jitter = 0.5x)
	minRand := func() float64 { return 0.0 }
	// Deterministic random func returning fixed 1.0 (maximum jitter = 1.0x)
	maxRand := func() float64 { return 1.0 }

	b1Min := exec.ComputeBackoff(1, minRand)
	b1Max := exec.ComputeBackoff(1, maxRand)

	if b1Min != 50*time.Millisecond {
		t.Errorf("expected attempt 1 min backoff 50ms, got %v", b1Min)
	}
	if b1Max != 100*time.Millisecond {
		t.Errorf("expected attempt 1 max backoff 100ms, got %v", b1Max)
	}

	b2Min := exec.ComputeBackoff(2, minRand)
	b2Max := exec.ComputeBackoff(2, maxRand)

	if b2Min != 100*time.Millisecond {
		t.Errorf("expected attempt 2 min backoff 100ms, got %v", b2Min)
	}
	if b2Max != 200*time.Millisecond {
		t.Errorf("expected attempt 2 max backoff 200ms, got %v", b2Max)
	}

	// Cap verification at high attempt number
	bHighMax := exec.ComputeBackoff(10, maxRand)
	if bHighMax != 2*time.Second {
		t.Errorf("expected capped backoff 2s, got %v", bHighMax)
	}
}
