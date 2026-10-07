package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCLI_PlanGitHubFormat(t *testing.T) {
	tmpDir := t.TempDir()
	dbFile := filepath.Join(tmpDir, "github_format.db")
	dsn := "sqlite:" + dbFile
	schemaFile := filepath.Join(tmpDir, "schema.sql")
	if err := os.WriteFile(schemaFile, []byte("CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT);"), 0600); err != nil {
		t.Fatalf("writing schema file: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	code := run([]string{"plan", "--dsn", dsn, "--schema", schemaFile, "--format", "github"})
	_ = w.Close()
	os.Stdout = oldStdout

	if code != 0 {
		t.Fatalf("expected exit 0 on plan --format github, got %d", code)
	}

	buf := make([]byte, 2048)
	n, _ := r.Read(buf)
	_ = r.Close()
	output := string(buf[:n])

	if !strings.Contains(output, "::notice title=Grizzle Migration Plan::") {
		t.Errorf("expected output to contain ::notice title=Grizzle Migration Plan::, got %q", output)
	}
}

func TestCLI_Init(t *testing.T) {
	origIsTerminal := isTerminalFunc
	defer func() { isTerminalFunc = origIsTerminal }()

	// Non-TTY environment
	isTerminalFunc = func(fd uintptr) bool { return false }

	tmpDir := t.TempDir()

	// 1. Missing --template in non-TTY -> exit 1
	if code := run([]string{"init", "--dir", tmpDir}); code != 1 {
		t.Fatalf("expected exit 1 on init without template in non-TTY, got %d", code)
	}

	// 2. Unknown template -> exit 1
	if code := run([]string{"init", "--dir", tmpDir, "--template", "invalid"}); code != 1 {
		t.Fatalf("expected exit 1 on invalid template, got %d", code)
	}

	// 3. Valid template: sqlc -> creates files
	initDir := filepath.Join(tmpDir, "sqlc_proj")
	if code := run([]string{"init", "--dir", initDir, "--template", "sqlc"}); code != 0 {
		t.Fatalf("expected exit 0 on init sqlc, got %d", code)
	}

	expectedFiles := []string{"schema.sql", "queries.sql", "sqlc.yaml", "main.go"}
	for _, f := range expectedFiles {
		if _, err := os.Stat(filepath.Join(initDir, f)); err != nil {
			t.Errorf("expected file %s to exist: %v", f, err)
		}
	}

	// 4. Running again without force -> exit 1
	if code := run([]string{"init", "--dir", initDir, "--template", "sqlc"}); code != 1 {
		t.Fatalf("expected exit 1 when files exist without --force, got %d", code)
	}

	// 5. Running with force -> exit 0
	if code := run([]string{"init", "--dir", initDir, "--template", "sqlc", "--force"}); code != 0 {
		t.Fatalf("expected exit 0 when using --force, got %d", code)
	}

	// 6. Valid template: sqlite -> creates schema.sql and main.go
	sqliteDir := filepath.Join(tmpDir, "sqlite_proj")
	if code := run([]string{"init", "--dir", sqliteDir, "--template", "sqlite"}); code != 0 {
		t.Fatalf("expected exit 0 on init sqlite, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(sqliteDir, "schema.sql")); err != nil {
		t.Errorf("expected schema.sql in sqlite_proj: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sqliteDir, "main.go")); err != nil {
		t.Errorf("expected main.go in sqlite_proj: %v", err)
	}
}

func TestRenameFlags_Parse(t *testing.T) {
	r := renameFlags{}
	cases := []string{"old_col=new_col", "users.old_col=new_name", "  spaced  =  trimmed  "}
	for _, c := range cases {
		if err := r.Set(c); err != nil {
			t.Fatalf("Set(%q) failed: %v", c, err)
		}
	}
	want := map[string]string{
		"old_col":       "new_col",
		"users.old_col": "new_name",
		"spaced":        "trimmed",
	}
	if len(r) != len(want) {
		t.Fatalf("expected %d entries, got %d: %v", len(want), len(r), r)
	}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("rename[%q] = %q, want %q", k, r[k], v)
		}
	}
}

func TestRenameFlags_RejectsInvalid(t *testing.T) {
	for _, c := range []string{"", "noequals", "=new_col", "old_col="} {
		r := renameFlags{}
		if err := r.Set(c); err == nil {
			t.Errorf("Set(%q) should fail", c)
		}
	}
}
