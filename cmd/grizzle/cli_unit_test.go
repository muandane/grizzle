package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCLI_RunDirect(t *testing.T) {
	// 1. Missing arguments -> exit 1
	if code := run([]string{}); code != 1 {
		t.Fatalf("expected exit 1 on empty args, got %d", code)
	}

	// 2. Version flags -> exit 0
	for _, v := range []string{"version", "--version", "-version", "-v"} {
		if code := run([]string{v}); code != 0 {
			t.Fatalf("expected exit 0 on %s, got %d", v, code)
		}
	}

	// 3. Unknown command -> exit 1
	if code := run([]string{"unknown_cmd"}); code != 1 {
		t.Fatalf("expected exit 1 on unknown command, got %d", code)
	}

	// 4. Missing DSN on plan -> exit 1
	origDSN := os.Getenv("DATABASE_URL")
	origGrizzleDSN := os.Getenv("GRIZZLE_DSN")
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Unsetenv("GRIZZLE_DSN")
	defer func() {
		if origDSN != "" {
			_ = os.Setenv("DATABASE_URL", origDSN)
		}
		if origGrizzleDSN != "" {
			_ = os.Setenv("GRIZZLE_DSN", origGrizzleDSN)
		}
	}()

	if code := run([]string{"plan"}); code != 1 {
		t.Fatalf("expected exit 1 on plan without DSN, got %d", code)
	}

	tmpDir := t.TempDir()
	dbFile := filepath.Join(tmpDir, "cli_direct.db")
	dsn := "sqlite:" + dbFile
	schemaFile := filepath.Join(tmpDir, "schema.sql")
	if err := os.WriteFile(schemaFile, []byte("CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT);"), 0600); err != nil {
		t.Fatalf("writing schema file: %v", err)
	}

	// 5. Plan with valid DSN and schema -> exit 0
	planFile := filepath.Join(tmpDir, "plan.json")
	if code := run([]string{"plan", "--dsn", dsn, "--schema", schemaFile, "--out", planFile}); code != 0 {
		t.Fatalf("expected exit 0 on plan, got %d", code)
	}

	// 6. Check before apply -> exit 4 (drift)
	if code := run([]string{"check", "--dsn", dsn, "--schema", schemaFile}); code != 4 {
		t.Fatalf("expected exit 4 on check with drift, got %d", code)
	}

	// 7. Apply plan -> exit 0
	if code := run([]string{"apply", "--dsn", dsn, "--plan", planFile}); code != 0 {
		t.Fatalf("expected exit 0 on apply, got %d", code)
	}

	// 8. Check after apply -> exit 0 (in sync)
	if code := run([]string{"check", "--dsn", dsn, "--schema", schemaFile}); code != 0 {
		t.Fatalf("expected exit 0 on check in sync, got %d", code)
	}

	// 9. Export sql, goose, and atlas
	for _, format := range []string{"sql", "goose", "atlas"} {
		exportDir := filepath.Join(tmpDir, "export_"+format)
		if code := run([]string{"export", "--dsn", dsn, "--schema", schemaFile, "--format", format, "--out", exportDir}); code != 0 {
			t.Fatalf("expected exit 0 on export %s, got %d", format, code)
		}
	}
}
