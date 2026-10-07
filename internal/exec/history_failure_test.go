package exec_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
)

// TestSyncSQLite_HistoryWriteFailure_TypedNonFatal verifies a successful
// migration whose history INSERT fails surfaces plan.ErrHistoryRecord (the
// schema changes stay committed) — SQLite allows a failed statement without
// poisoning the surrounding transaction.
func TestSyncSQLite_HistoryWriteFailure_TypedNonFatal(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	// Poison grizzle_history: EnsureTable no-ops (columns exist), the
	// INSERT violates the status CHECK.
	if _, err := db.Exec(`CREATE TABLE grizzle_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		plan_hash TEXT,
		status TEXT NOT NULL CHECK (status <> 'applied'),
		failed_step INTEGER,
		error TEXT,
		applied_at TIMESTAMP,
		duration_ms INTEGER,
		applied_by TEXT,
		steps TEXT,
		seed_hash TEXT
	);`); err != nil {
		t.Fatalf("failed creating poisoned history table: %v", err)
	}

	cfg := exec.SQLiteExecConfig{
		SchemaSQL: "CREATE TABLE hist_fail_tbl (id INTEGER PRIMARY KEY);",
	}

	err := exec.SyncSQLite(ctx, db, cfg)
	if err == nil {
		t.Fatalf("expected typed non-fatal history error, got nil")
	}
	if !errors.Is(err, plan.ErrHistoryRecord) {
		t.Errorf("expected ErrHistoryRecord, got: %v", err)
	}

	// The migration itself must have been applied.
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='hist_fail_tbl';`).Scan(&count); err != nil {
		t.Fatalf("querying hist_fail_tbl: %v", err)
	}
	if count != 1 {
		t.Errorf("table was not applied despite non-fatal history error")
	}
}

// TestSyncSQLite_HookPanic_HistoryFirstLineAndLogger verifies a hook panic:
// the returned error and the logger carry the full debug.Stack(), while the
// grizzle_history error column records the first line only.
func TestSyncSQLite_HookPanic_HistoryFirstLineAndLogger(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	logBuf := &capturingLogs{}
	cfg := exec.SQLiteExecConfig{
		SchemaSQL: `
			CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);
			CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users (id));
		`,
		Logger: logBuf.logger(),
		BeforeStep: func(hc exec.HookContext) error {
			panic("stack trace kaboom")
		},
	}

	err := exec.SyncSQLite(ctx, db, cfg)
	if err == nil {
		t.Fatalf("expected panic converted to error, got nil")
	}
	if !strings.Contains(err.Error(), "goroutine") || !strings.Contains(err.Error(), "stack trace kaboom") {
		t.Errorf("returned error must carry the full stack, got: %v", err)
	}

	// Logger saw the panic error (with stack).
	if !strings.Contains(logBuf.buf.String(), "grizzle: hook failed") || !strings.Contains(logBuf.buf.String(), "goroutine") {
		t.Errorf("logger must receive the panic error with stack, got: %q", logBuf.buf.String())
	}

	// History keeps the first line only.
	var histErr string
	if err := db.QueryRow(`SELECT COALESCE(error, '') FROM grizzle_history ORDER BY id DESC LIMIT 1`).Scan(&histErr); err != nil {
		t.Fatalf("querying history: %v", err)
	}
	if !strings.Contains(histErr, "stack trace kaboom") {
		t.Errorf("history error missing panic message: %q", histErr)
	}
	if strings.Contains(histErr, "goroutine") {
		t.Errorf("history error must not contain the stack trace: %q", histErr)
	}
}
