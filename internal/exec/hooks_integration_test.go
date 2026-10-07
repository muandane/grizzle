//go:build integration

package exec_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/testutil"
)

func uniqueHookSchema(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test_hooks_%d", time.Now().UnixNano())
}

func dropSchema(t *testing.T, db *sql.DB, schema string) {
	t.Helper()
	_, _ = db.ExecContext(context.Background(), fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
}

func TestHooks_Postgres_StepOrder(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := uniqueHookSchema(t)
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}

	var order []string
	var hookTotal int
	cfg := exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL: `
			CREATE TABLE users (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, email TEXT NOT NULL);
			CREATE TABLE posts (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, user_id INTEGER REFERENCES users (id));
		`,
		BeforeSync: func(ctx context.Context, dbtx dialect.DBTX) error {
			order = append(order, "before_sync")
			return nil
		},
		BeforeStep: func(hc exec.HookContext) error {
			order = append(order, fmt.Sprintf("before_step_%d", hc.Index))
			hookTotal = hc.Total
			return nil
		},
		AfterStep: func(hc exec.HookContext) error {
			order = append(order, fmt.Sprintf("after_step_%d", hc.Index))
			return nil
		},
		AfterSync: func(ctx context.Context, dbtx dialect.DBTX) error {
			order = append(order, "after_sync")
			return nil
		},
	}

	if err := exec.SyncPostgres(ctx, db, cfg); err != nil {
		t.Fatalf("SyncPostgres failed: %v", err)
	}

	want := []string{"before_sync"}
	beforeCount := 0
	afterCount := 0
	for _, e := range order {
		if strings.HasPrefix(e, "before_step_") {
			beforeCount++
		}
		if strings.HasPrefix(e, "after_step_") {
			afterCount++
		}
	}
	for i := 1; i <= beforeCount; i++ {
		want = append(want, fmt.Sprintf("before_step_%d", i), fmt.Sprintf("after_step_%d", i))
	}
	want = append(want, "after_sync")
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("hook order = %v, want %v", order, want)
	}
	if beforeCount != afterCount || beforeCount == 0 {
		t.Fatalf("step hook invocation mismatch: %d before vs %d after", beforeCount, afterCount)
	}
	if hookTotal != beforeCount {
		t.Errorf("HookContext.Total = %d, want %d", hookTotal, beforeCount)
	}

	var tableCount int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = '%s' AND table_name IN ('users','posts')`, schema,
	)).Scan(&tableCount); err != nil {
		t.Fatalf("querying tables: %v", err)
	}
	if tableCount != 2 {
		t.Errorf("tables created = %d, want 2", tableCount)
	}
}

func TestHooks_Postgres_BeforeStepFailureRollsBack(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := uniqueHookSchema(t)
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}

	cfg := exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL: `
			CREATE TABLE users (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, email TEXT NOT NULL);
			CREATE TABLE posts (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, user_id INTEGER REFERENCES users (id));
		`,
		BeforeStep: func(hc exec.HookContext) error {
			if hc.Index == 2 {
				return errors.New("hook boom")
			}
			return nil
		},
	}

	err := exec.SyncPostgres(ctx, db, cfg)
	if err == nil || !strings.Contains(err.Error(), "before_step hook:") {
		t.Fatalf("expected before_step hook error, got %v", err)
	}

	var tableCount int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = '%s' AND table_name IN ('users','posts')`, schema,
	)).Scan(&tableCount); err != nil {
		t.Fatalf("querying tables: %v", err)
	}
	if tableCount != 0 {
		t.Errorf("tables visible after rollback = %d, want 0", tableCount)
	}

	assertPostgresHistory(t, db, schema, "before_step hook:", 2, "failed")
}

func TestHooks_Postgres_AfterSyncFailure(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := uniqueHookSchema(t)
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}

	cfg := exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL: `
			CREATE TABLE users (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, email TEXT NOT NULL);
		`,
		AfterSync: func(ctx context.Context, dbtx dialect.DBTX) error {
			return errors.New("hook boom")
		},
	}

	err := exec.SyncPostgres(ctx, db, cfg)
	if !errors.Is(err, plan.ErrAfterSyncFailed) {
		t.Fatalf("expected ErrAfterSyncFailed, got %v", err)
	}

	var tableCount int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = '%s' AND table_name = 'users'`, schema,
	)).Scan(&tableCount); err != nil {
		t.Fatalf("querying tables: %v", err)
	}
	if tableCount != 1 {
		t.Errorf("users table must remain committed, got %d", tableCount)
	}
}

func assertPostgresHistory(t *testing.T, db *sql.DB, schema, errSubstring string, wantFailedStep int, wantStatus string) {
	t.Helper()
	var status string
	var failedStep int
	var histErr string
	err := db.QueryRowContext(context.Background(), fmt.Sprintf(
		`SELECT status, COALESCE(failed_step, 0), COALESCE(error, '') FROM %s.grizzle_history ORDER BY id DESC LIMIT 1`, schema,
	)).Scan(&status, &failedStep, &histErr)
	if err != nil {
		t.Fatalf("querying history: %v", err)
	}
	if status != wantStatus {
		t.Errorf("history status = %q, want %q", status, wantStatus)
	}
	if failedStep != wantFailedStep {
		t.Errorf("history failed_step = %d, want %d", failedStep, wantFailedStep)
	}
	if !strings.Contains(histErr, errSubstring) {
		t.Errorf("history error = %q, want substring %q", histErr, errSubstring)
	}
}
