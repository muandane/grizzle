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

// TestErrorChain_Characterization asserts the current behavior of error wrapping:
// Sentinel errors match via errors.Is, but driver errors (*pgconn.PgError) are NOT unwrap-able
// via errors.As because %v is currently used instead of %w.
// In Step 3, when upgraded to %w: %w, errors.As will flip to true.
func TestErrorChain_Characterization(t *testing.T) {
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

	// 1. Sentinel matches
	if !errors.Is(err, plan.ErrInspectionFailed) {
		t.Errorf("expected errors.Is(err, plan.ErrInspectionFailed) to be true, got false: %v", err)
	}

	// 2. Driver error does NOT unwrap under current %v wrapping (characterization of legacy behavior)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Errorf("expected errors.As(err, &pgErr) to be false on current %%%%v wrapping, but was true")
	}
}
