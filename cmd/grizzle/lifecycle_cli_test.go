package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muandane/grizzle/internal/history"
)

func TestCLI_LifecycleCommands(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	dsn := "sqlite:" + dbPath

	schemaFile := filepath.Join(tmpDir, "schema.sql")
	initialSQL := "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL);\n"
	if err := os.WriteFile(schemaFile, []byte(initialSQL), 0600); err != nil {
		t.Fatalf("failed writing schema file: %v", err)
	}

	// 1. Initial Status on unapplied DB should exit code 1 (drift detected)
	code := run([]string{"status", "--dsn", dsn, "--schema", schemaFile})
	if code != 1 {
		t.Errorf("expected exit code 1 for status with unapplied drift, got %d", code)
	}

	// Initial Status with --json should output valid JSON and exit 1
	code = run([]string{"status", "--dsn", dsn, "--schema", schemaFile, "--json"})
	if code != 1 {
		t.Errorf("expected exit code 1 for json status with unapplied drift, got %d", code)
	}

	// 2. Baseline without --force should fail because DB is empty while schema.sql has users table
	code = run([]string{"baseline", "--dsn", dsn, "--schema", schemaFile, "--note", "initial baseline"})
	if code != 1 {
		t.Errorf("expected exit code 1 for baseline with drift without --force, got %d", code)
	}

	// 3. Baseline with --force should succeed (exit 0)
	code = run([]string{"baseline", "--dsn", dsn, "--schema", schemaFile, "--note", "forced baseline", "--force"})
	if code != 0 {
		t.Errorf("expected exit code 0 for forced baseline, got %d", code)
	}

	// 4. History command should show the baseline entry (exit 0)
	code = run([]string{"history", "--dsn", dsn})
	if code != 0 {
		t.Errorf("expected exit code 0 for history, got %d", code)
	}

	code = run([]string{"history", "--dsn", dsn, "--json"})
	if code != 0 {
		t.Errorf("expected exit code 0 for json history, got %d", code)
	}

	// 5. Repair when no failures exist should exit 0 and report none found
	code = run([]string{"repair", "--dsn", dsn})
	if code != 0 {
		t.Errorf("expected exit code 0 for repair with no failures, got %d", code)
	}

	// 6. Simulate a failed migration in history
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	err = history.Insert(ctx, db, "sqlite", "", history.Record{
		PlanHash:   "fail_hash",
		Status:     "failed",
		FailedStep: 2,
		Error:      "syntax error",
		AppliedAt:  time.Now().UTC(),
		DurationMs: 10,
		AppliedBy:  "testuser",
		StepsJSON:  "[]",
	})
	if err != nil {
		t.Fatalf("failed inserting simulated failed migration: %v", err)
	}
	_ = db.Close()

	// Repair should now mark it resolved (exit 0)
	code = run([]string{"repair", "--dsn", dsn, "--note", "fixed by operator"})
	if code != 0 {
		t.Errorf("expected exit code 0 for repair, got %d", code)
	}

	// Second repair should report none found (exit 0)
	code = run([]string{"repair", "--dsn", dsn})
	if code != 0 {
		t.Errorf("expected exit code 0 for second repair, got %d", code)
	}

	// 7. Dump command
	dumpFile := filepath.Join(tmpDir, "dump.sql")
	code = run([]string{"dump", "--dsn", dsn, "--out", dumpFile})
	if code != 0 {
		t.Errorf("expected exit code 0 for dump, got %d", code)
	}

	// Dump stdout
	code = run([]string{"dump", "--dsn", dsn})
	if code != 0 {
		t.Errorf("expected exit code 0 for dump to stdout, got %d", code)
	}
}

func TestCLI_ConfigFileAndSyncHooks(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "hooks.db")
	dsn := "sqlite:" + dbPath

	schemaFile := filepath.Join(tmpDir, "schema.sql")
	if err := os.WriteFile(schemaFile, []byte("CREATE TABLE audit (id INTEGER PRIMARY KEY, note TEXT);\n"), 0600); err != nil {
		t.Fatalf("failed writing schema: %v", err)
	}

	beforeFile := filepath.Join(tmpDir, "before.sql")
	if err := os.WriteFile(beforeFile, []byte("CREATE TABLE IF NOT EXISTS hook_log (msg TEXT); INSERT INTO hook_log VALUES ('before');\n"), 0600); err != nil {
		t.Fatalf("failed writing before hook: %v", err)
	}

	afterFile := filepath.Join(tmpDir, "after.sql")
	if err := os.WriteFile(afterFile, []byte("INSERT INTO hook_log VALUES ('after');\n"), 0600); err != nil {
		t.Fatalf("failed writing after hook: %v", err)
	}

	yamlFile := filepath.Join(tmpDir, "grizzle.yaml")
	yamlContent := "dsn: \"" + dsn + "\"\nschema: \"" + schemaFile + "\"\nbefore_sync: \"" + beforeFile + "\"\nafter_sync: \"" + afterFile + "\"\n"
	if err := os.WriteFile(yamlFile, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("failed writing yaml: %v", err)
	}

	// 1. Run plan with --config
	planFile := filepath.Join(tmpDir, "plan.json")
	code := run([]string{"plan", "--config", yamlFile, "--out", planFile})
	if code != 0 {
		t.Fatalf("expected exit code 0 for plan with config, got %d", code)
	}

	// 2. Run apply with --config
	code = run([]string{"apply", "--config", yamlFile, "--plan", planFile})
	if code != 0 {
		t.Fatalf("expected exit code 0 for apply with config and hooks, got %d", code)
	}

	// Verify database has both audit table and hook_log entries
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	var hookCount int
	err = db.QueryRow("SELECT count(*) FROM hook_log").Scan(&hookCount)
	if err != nil {
		t.Fatalf("querying hook_log: %v", err)
	}
	if hookCount != 2 {
		t.Errorf("expected 2 hook_log entries (before and after), got %d", hookCount)
	}
}
