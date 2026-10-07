package exec_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
)

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSyncSQLite_HookOrder(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);`); err != nil {
		t.Fatalf("seeding users table: %v", err)
	}

	var order []string
	cfg := exec.SQLiteExecConfig{
		SchemaSQL: `
			CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);
			CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users (id));
		`,
		BeforeSync: func(ctx context.Context, dbtx dialect.DBTX) error {
			if dbtx == nil {
				t.Error("BeforeSync received nil DBTX")
			}
			order = append(order, "before_sync")
			return nil
		},
		BeforeStep: func(hc exec.HookContext) error {
			order = append(order, fmt.Sprintf("before_step_%d", hc.Index))
			if hc.Total != 1 {
				t.Errorf("BeforeStep Total = %d, want 1", hc.Total)
			}
			if hc.IsNonTx {
				t.Error("BeforeStep IsNonTx = true, want false")
			}
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

	if err := exec.SyncSQLite(ctx, db, cfg); err != nil {
		t.Fatalf("SyncSQLite failed: %v", err)
	}

	want := []string{"before_sync", "before_step_1", "after_step_1", "after_sync"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("hook order = %v, want %v", order, want)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM posts;`).Scan(&count); err != nil {
		t.Fatalf("posts table missing: %v", err)
	}
}

func TestSyncSQLite_BeforeStepFailure(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);`); err != nil {
		t.Fatalf("seeding users table: %v", err)
	}

	cfg := exec.SQLiteExecConfig{
		SchemaSQL: `
			CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);
			CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users (id));
		`,
		BeforeStep: func(hc exec.HookContext) error {
			return errors.New("hook boom")
		},
	}

	err := exec.SyncSQLite(ctx, db, cfg)
	if err == nil || !strings.Contains(err.Error(), "before_step hook:") {
		t.Fatalf("expected before_step hook error, got %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='posts';`).Scan(&count); err != nil {
		t.Fatalf("querying posts existence: %v", err)
	}
	if count != 0 {
		t.Error("posts table should not exist after before_step hook failure")
	}

	assertSQLiteHistory(t, db, "before_step hook:", 1)
}

func TestSyncSQLite_AfterStepFailure(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);`); err != nil {
		t.Fatalf("seeding users table: %v", err)
	}

	cfg := exec.SQLiteExecConfig{
		SchemaSQL: `
			CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);
			CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users (id));
		`,
		AfterStep: func(hc exec.HookContext) error {
			return errors.New("hook boom")
		},
	}

	err := exec.SyncSQLite(ctx, db, cfg)
	if err == nil || !strings.Contains(err.Error(), "after_step hook:") {
		t.Fatalf("expected after_step hook error, got %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='posts';`).Scan(&count); err != nil {
		t.Fatalf("querying posts existence: %v", err)
	}
	if count != 0 {
		t.Error("posts table should be rolled back after after_step hook failure")
	}

	assertSQLiteHistory(t, db, "after_step hook:", 1)
}

func TestSyncSQLite_AfterSyncFailure(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);`); err != nil {
		t.Fatalf("seeding users table: %v", err)
	}

	cfg := exec.SQLiteExecConfig{
		SchemaSQL: `
			CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);
			CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users (id));
		`,
		AfterSync: func(ctx context.Context, dbtx dialect.DBTX) error {
			return errors.New("hook boom")
		},
	}

	err := exec.SyncSQLite(ctx, db, cfg)
	if !errors.Is(err, plan.ErrAfterSyncFailed) {
		t.Fatalf("expected ErrAfterSyncFailed, got %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='posts';`).Scan(&count); err != nil {
		t.Fatalf("querying posts existence: %v", err)
	}
	if count != 1 {
		t.Error("posts table must remain committed after after_sync hook failure")
	}
}

func assertSQLiteHistory(t *testing.T, db *sql.DB, errSubstring string, wantFailedStep int) {
	t.Helper()
	var status string
	var failedStep int
	var histErr string
	err := db.QueryRow(
		`SELECT status, COALESCE(failed_step, 0), COALESCE(error, '') FROM grizzle_history ORDER BY id DESC LIMIT 1`,
	).Scan(&status, &failedStep, &histErr)
	if err != nil {
		t.Fatalf("querying history: %v", err)
	}
	if status != "failed" && status != "partial" {
		t.Errorf("history status = %q, want failed/partial", status)
	}
	if failedStep != wantFailedStep {
		t.Errorf("history failed_step = %d, want %d", failedStep, wantFailedStep)
	}
	if !strings.Contains(histErr, errSubstring) {
		t.Errorf("history error = %q, want substring %q", histErr, errSubstring)
	}
}
