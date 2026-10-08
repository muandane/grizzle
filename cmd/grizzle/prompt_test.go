package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muandane/grizzle"
)

func TestPromptInteractiveApply_Yes(t *testing.T) {
	p := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{
				Type:  grizzle.ChangeCreateTable,
				Table: "accounts",
				SQL:   "CREATE TABLE accounts (id int, balance numeric);",
			},
		},
	}

	in := strings.NewReader("y\n")
	var out bytes.Buffer

	confirmed, err := promptInteractiveApply(in, &out, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !confirmed {
		t.Fatalf("expected confirmed=true on 'y'")
	}
	outStr := out.String()
	if !strings.Contains(outStr, "Planned changes:") {
		t.Errorf("output missing 'Planned changes:', got: %s", outStr)
	}
	if !strings.Contains(outStr, "Apply these changes? [y/N/details]: ") {
		t.Errorf("output missing prompt, got: %s", outStr)
	}
	if strings.Contains(outStr, "Migration aborted.") {
		t.Errorf("output should not contain 'Migration aborted.', got: %s", outStr)
	}
}

func TestPromptInteractiveApply_CaseInsensitiveYes(t *testing.T) {
	p := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{
				Type:  grizzle.ChangeCreateTable,
				Table: "accounts",
				SQL:   "CREATE TABLE accounts (id int);",
			},
		},
	}

	in := strings.NewReader("  YES  \n")
	var out bytes.Buffer

	confirmed, err := promptInteractiveApply(in, &out, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !confirmed {
		t.Fatalf("expected confirmed=true on 'YES'")
	}
}

func TestPromptInteractiveApply_No(t *testing.T) {
	p := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{
				Type:  grizzle.ChangeCreateTable,
				Table: "accounts",
				SQL:   "CREATE TABLE accounts (id int);",
			},
		},
	}

	in := strings.NewReader("n\n")
	var out bytes.Buffer

	confirmed, err := promptInteractiveApply(in, &out, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if confirmed {
		t.Fatalf("expected confirmed=false on 'n'")
	}
	if !strings.Contains(out.String(), "Migration aborted.") {
		t.Errorf("expected 'Migration aborted.', got: %s", out.String())
	}
}

func TestPromptInteractiveApply_EmptyInput(t *testing.T) {
	p := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{
				Type:  grizzle.ChangeCreateTable,
				Table: "accounts",
				SQL:   "CREATE TABLE accounts (id int);",
			},
		},
	}

	in := strings.NewReader("\n")
	var out bytes.Buffer

	confirmed, err := promptInteractiveApply(in, &out, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if confirmed {
		t.Fatalf("expected confirmed=false on empty input")
	}
	if !strings.Contains(out.String(), "Migration aborted.") {
		t.Errorf("expected 'Migration aborted.', got: %s", out.String())
	}
}

func TestPromptInteractiveApply_EOF(t *testing.T) {
	p := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{
				Type:  grizzle.ChangeCreateTable,
				Table: "accounts",
				SQL:   "CREATE TABLE accounts (id int);",
			},
		},
	}

	in := strings.NewReader("")
	var out bytes.Buffer

	confirmed, err := promptInteractiveApply(in, &out, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if confirmed {
		t.Fatalf("expected confirmed=false on EOF")
	}
	if !strings.Contains(out.String(), "Migration aborted.") {
		t.Errorf("expected 'Migration aborted.', got: %s", out.String())
	}
}

func TestPromptInteractiveApply_DetailsThenYes(t *testing.T) {
	p := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{
				Type:        grizzle.ChangeDropColumn,
				Table:       "users",
				Column:      "legacy_role",
				SQL:         `ALTER TABLE "users" DROP COLUMN "legacy_role";`,
				Destructive: true,
			},
		},
		Policy: grizzle.DropPolicy{
			AllowColumn: true,
		},
	}

	in := strings.NewReader("details\nyes\n")
	var out bytes.Buffer

	confirmed, err := promptInteractiveApply(in, &out, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !confirmed {
		t.Fatalf("expected confirmed=true on 'details' -> 'yes'")
	}

	outStr := out.String()
	if !strings.Contains(outStr, "Full SQL statements:") {
		t.Errorf("missing 'Full SQL statements:', got: %s", outStr)
	}
	if !strings.Contains(outStr, `ALTER TABLE "users" DROP COLUMN "legacy_role";`) {
		t.Errorf("missing full SQL statement, got: %s", outStr)
	}
	if !strings.Contains(outStr, "Apply these changes? [y/N]: ") {
		t.Errorf("missing secondary prompt, got: %s", outStr)
	}
}

func TestPromptInteractiveApply_DetailsThenNo(t *testing.T) {
	p := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{
				Type:        grizzle.ChangeDropColumn,
				Table:       "users",
				Column:      "legacy_role",
				SQL:         `ALTER TABLE "users" DROP COLUMN "legacy_role";`,
				Destructive: true,
			},
		},
		Policy: grizzle.DropPolicy{
			AllowColumn: true,
		},
	}

	in := strings.NewReader("d\nno\n")
	var out bytes.Buffer

	confirmed, err := promptInteractiveApply(in, &out, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if confirmed {
		t.Fatalf("expected confirmed=false on 'd' -> 'no'")
	}
	if !strings.Contains(out.String(), "Migration aborted.") {
		t.Errorf("expected 'Migration aborted.', got: %s", out.String())
	}
}

func TestPromptInteractiveApply_EmptyPlan(t *testing.T) {
	p := &grizzle.Plan{
		TargetSchema: "public",
		Steps:        nil,
	}

	in := strings.NewReader("y\n")
	var out bytes.Buffer

	confirmed, err := promptInteractiveApply(in, &out, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if confirmed {
		t.Fatalf("expected confirmed=false on empty plan")
	}
	if !strings.Contains(out.String(), "Database schema is already in sync.") {
		t.Errorf("expected in sync message, got: %s", out.String())
	}
}

func TestRunApply_InteractiveTerminalSimulated(t *testing.T) {
	tmpDir := t.TempDir()
	dbFile := filepath.Join(tmpDir, "simulated_interactive.db")
	dsn := "sqlite:" + dbFile

	// 1. Initial schema: table with 2 columns
	schemaV1 := filepath.Join(tmpDir, "schema_v1.sql")
	if err := os.WriteFile(schemaV1, []byte("CREATE TABLE users (id INTEGER PRIMARY KEY, legacy_role TEXT);"), 0600); err != nil {
		t.Fatalf("writing schemaV1: %v", err)
	}

	// Apply v1 non-interactively
	if code := runApply(context.Background(), dsn, "", schemaV1, "", "", "", false, nil, false, nil, false, nil); code != 0 {
		t.Fatalf("expected initial apply exit 0, got %d", code)
	}

	// 2. Destructive schema: drop column legacy_role
	schemaV2 := filepath.Join(tmpDir, "schema_v2.sql")
	if err := os.WriteFile(schemaV2, []byte("CREATE TABLE users (id INTEGER PRIMARY KEY);"), 0600); err != nil {
		t.Fatalf("writing schemaV2: %v", err)
	}

	// Case A: Non-interactive / headless environment without --accept-hazard -> exit code 2
	origIsTerminal := isTerminalFunc
	defer func() { isTerminalFunc = origIsTerminal }()
	isTerminalFunc = func(fd uintptr) bool { return false }

	codeBlocked := runApply(context.Background(), dsn, "", schemaV2, "", "", "", true, nil, false, nil, false, nil)
	if codeBlocked != 2 {
		t.Fatalf("expected exit code 2 in headless mode without accepted hazard, got %d", codeBlocked)
	}

	// Case B: Interactive terminal simulated with user answering 'n' -> exit code 1 (aborted)
	isTerminalFunc = func(fd uintptr) bool { return true }

	// Simulate stdin with 'n'
	origStdin := os.Stdin
	rNo, wNo, _ := os.Pipe()
	os.Stdin = rNo
	_, _ = wNo.WriteString("n\n")
	_ = wNo.Close()

	codeAborted := runApply(context.Background(), dsn, "", schemaV2, "", "", "", true, nil, false, nil, false, nil)
	_ = rNo.Close()
	os.Stdin = origStdin

	if codeAborted != 1 {
		t.Fatalf("expected exit code 1 when user aborts with 'n', got %d", codeAborted)
	}

	// Case C: Interactive terminal simulated with user answering 'y' -> exit code 0 (applied, hazards accepted!)
	rYes, wYes, _ := os.Pipe()
	os.Stdin = rYes
	_, _ = wYes.WriteString("y\n")
	_ = wYes.Close()

	codeSuccess := runApply(context.Background(), dsn, "", schemaV2, "", "", "", true, nil, false, nil, false, nil)
	_ = rYes.Close()
	os.Stdin = origStdin

	if codeSuccess != 0 {
		t.Fatalf("expected exit code 0 when user confirms with 'y', got %d", codeSuccess)
	}

	// Case D: Re-running apply now that schema is in sync -> exit code 0 (reports in sync)
	rSync, wSync, _ := os.Pipe()
	os.Stdin = rSync
	_ = wSync.Close()

	codeInSync := runApply(context.Background(), dsn, "", schemaV2, "", "", "", true, nil, false, nil, false, nil)
	_ = rSync.Close()
	os.Stdin = origStdin

	if codeInSync != 0 {
		t.Fatalf("expected exit code 0 when already in sync, got %d", codeInSync)
	}
}

func TestPromptInteractiveInit(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expected    string
		expectError bool
	}{
		{name: "default empty enters sqlc", input: "\n", expected: "sqlc"},
		{name: "choice 1 enters sqlc", input: "1\n", expected: "sqlc"},
		{name: "name sqlc", input: "sqlc\n", expected: "sqlc"},
		{name: "choice 2 enters stdlib", input: "2\n", expected: "stdlib"},
		{name: "name stdlib", input: "stdlib\n", expected: "stdlib"},
		{name: "choice 3 enters sqlite", input: "3\n", expected: "sqlite"},
		{name: "name sqlite", input: "sqlite\n", expected: "sqlite"},
		{name: "invalid choice", input: "4\n", expectError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := strings.NewReader(tc.input)
			var out bytes.Buffer
			template, err := promptInteractiveInit(in, &out)
			if tc.expectError {
				if err == nil {
					t.Fatalf("expected error on input %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error on input %q: %v", tc.input, err)
			}
			if template != tc.expected {
				t.Errorf("expected template %q, got %q", tc.expected, template)
			}
		})
	}
}
