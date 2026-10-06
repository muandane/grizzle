package exec_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
)

type mockFailingDBTX struct {
	err error
}

func (m *mockFailingDBTX) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return nil, m.err
}

func (m *mockFailingDBTX) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return nil, m.err
}

func (m *mockFailingDBTX) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return nil
}

// TestErrorChain_PreservesDriverError asserts that error wrapping with %w: %w preserves both
// the sentinel error (for high-level categorization) and the underlying driver error (*pgconn.PgError).
func TestErrorChain_PreservesDriverError(t *testing.T) {
	driverErr := &pgconn.PgError{
		Code:    "42P01",
		Message: "relation does not exist",
	}

	dbtx := &mockFailingDBTX{err: driverErr}
	cfg := exec.PostgresExecConfig{
		TargetSchema: "public",
	}

	_, err := exec.DiffPostgres(context.Background(), dbtx, cfg)
	if err == nil {
		t.Fatalf("expected error from DiffPostgres, got nil")
	}

	// 1. Sentinel matches via errors.Is
	if !errors.Is(err, plan.ErrInspectionFailed) {
		t.Errorf("expected errors.Is(err, plan.ErrInspectionFailed) to be true, got false: %v", err)
	}

	// 2. Driver error unwraps via errors.As (*pgconn.PgError)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected errors.As(err, &pgErr) to be true with %%w: %%w wrapping, got false")
	}
	if pgErr.Code != "42P01" {
		t.Errorf("expected pgErr.Code to be 42P01, got %s", pgErr.Code)
	}
}
