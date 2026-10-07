package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "grizzle")

	buildCmd := exec.Command("go", "build", "-o", binPath, ".") //nolint:gosec // G204: test builds CLI
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed building CLI: %v\nOutput: %s", err, string(out))
	}
	return binPath
}

func TestCLI_ExitCode1_Errors(t *testing.T) {
	binPath := buildBinary(t)

	// 1. Run without arguments -> exit 1
	cmd := exec.Command(binPath) //nolint:gosec // G204: test executes CLI
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("expected error running without args, got success")
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Errorf("expected usage output, got: %s", string(out))
	}

	// 2. Unknown command -> exit 1
	cmdUnknown := exec.Command(binPath, "unknown_cmd", "--dsn", "sqlite::memory:") //nolint:gosec // G204: test executes CLI
	_, err = cmdUnknown.CombinedOutput()
	if err == nil {
		t.Errorf("expected error running unknown command, got success")
	}

	// 3. Plan without DSN -> exit 1
	cmdNoDSN := exec.Command(binPath, "plan") //nolint:gosec // G204: test executes CLI
	cmdNoDSN.Env = []string{"PATH=" + os.Getenv("PATH")}
	_, err = cmdNoDSN.CombinedOutput()
	if err == nil {
		t.Errorf("expected error running plan without DSN, got success")
	}
}

func TestCLI_Workflow_ExitCodes(t *testing.T) {
	binPath := buildBinary(t)
	tmpDir := t.TempDir()

	dbFile := filepath.Join(tmpDir, "test.db")
	dsn := "sqlite:" + dbFile

	// 1. Initial schema with table
	schemaV1 := filepath.Join(tmpDir, "schema_v1.sql")
	if err := os.WriteFile(schemaV1, []byte("CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);"), 0600); err != nil {
		t.Fatalf("writing schemaV1: %v", err)
	}

	// Exit Code 0: Plan --out plan.json
	planFile := filepath.Join(tmpDir, "plan.json")
	cmdPlan := exec.Command(binPath, "plan", "--dsn", dsn, "--schema", schemaV1, "--out", planFile, "--json-log") //nolint:gosec // G204: test executes CLI
	out, err := cmdPlan.CombinedOutput()
	if err != nil {
		t.Fatalf("grizzle plan failed (exit != 0): %v\nOutput: %s", err, string(out))
	}

	planContent, err := os.ReadFile(planFile) //nolint:gosec // G304: test reads generated plan output
	if err != nil {
		t.Fatalf("reading plan output: %v", err)
	}
	if !strings.Contains(string(planContent), "users") || !strings.Contains(string(planContent), "hash") {
		t.Fatalf("unexpected plan JSON content: %s", string(planContent))
	}

	// Exit Code 4: Check detects drift before apply
	cmdCheckDrift := exec.Command(binPath, "check", "--dsn", dsn, "--schema", schemaV1) //nolint:gosec // G204: test executes CLI
	out, err = cmdCheckDrift.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 4 {
		t.Fatalf("expected exit code 4 on drift, got err: %v\nOutput: %s", err, string(out))
	}

	// Exit Code 0: Apply with --plan
	cmdApply := exec.Command(binPath, "apply", "--dsn", dsn, "--plan", planFile) //nolint:gosec // G204: test executes CLI
	out, err = cmdApply.CombinedOutput()
	if err != nil {
		t.Fatalf("grizzle apply failed (exit != 0): %v\nOutput: %s", err, string(out))
	}

	// Exit Code 0: Check returns 0 clean after apply
	cmdCheckClean := exec.Command(binPath, "check", "--dsn", dsn, "--schema", schemaV1) //nolint:gosec // G204: test executes CLI
	out, err = cmdCheckClean.CombinedOutput()
	if err != nil {
		t.Fatalf("grizzle check clean failed (exit != 0): %v\nOutput: %s", err, string(out))
	}

	// Exit Code 3: Applying same approved plan again detects drift (db is already up to date)
	cmdApplyAgain := exec.Command(binPath, "apply", "--dsn", dsn, "--plan", planFile) //nolint:gosec // G204: test executes CLI
	out, err = cmdApplyAgain.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("expected exit code 3 on plan drift, got err: %v\nOutput: %s", err, string(out))
	}

	// 2. Destructive schema change: drop column email
	schemaV2 := filepath.Join(tmpDir, "schema_v2.sql")
	if err := os.WriteFile(schemaV2, []byte("CREATE TABLE users (id INTEGER PRIMARY KEY);"), 0600); err != nil {
		t.Fatalf("writing schemaV2: %v", err)
	}

	planFileV2 := filepath.Join(tmpDir, "plan_v2.json")
	cmdPlanV2 := exec.Command(binPath, "plan", "--dsn", dsn, "--schema", schemaV2, "--allow-drop", "--out", planFileV2) //nolint:gosec // G204: test executes CLI
	if out, err := cmdPlanV2.CombinedOutput(); err != nil {
		t.Fatalf("grizzle plan v2 failed: %v\nOutput: %s", err, string(out))
	}

	// Exit Code 2: Apply without --accept-hazard DROP_COLUMN fails with exit code 2
	cmdApplyBlocked := exec.Command(binPath, "apply", "--dsn", dsn, "--plan", planFileV2, "--allow-drop") //nolint:gosec // G204: test executes CLI
	out, err = cmdApplyBlocked.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit code 2 on hazard block, got err: %v\nOutput: %s", err, string(out))
	}

	// Exit Code 0: Apply with --accept-hazard DROP_COLUMN succeeds
	cmdApplyAccepted := exec.Command(binPath, "apply", "--dsn", dsn, "--plan", planFileV2, "--allow-drop", "--accept-hazard", "DROP_COLUMN") //nolint:gosec // G204: test executes CLI
	if out, err := cmdApplyAccepted.CombinedOutput(); err != nil {
		t.Fatalf("expected apply with accepted hazard to succeed: %v\nOutput: %s", err, string(out))
	}
}

func TestCLI_Export(t *testing.T) {
	binPath := buildBinary(t)
	tmpDir := t.TempDir()

	schemaFile := filepath.Join(tmpDir, "schema.sql")
	if err := os.WriteFile(schemaFile, []byte("CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT);"), 0600); err != nil {
		t.Fatalf("writing schema: %v", err)
	}

	exportDir := filepath.Join(tmpDir, "exports")

	// Export Goose format
	cmdGoose := exec.Command(binPath, "export", "--dsn", "sqlite::memory:", "--schema", schemaFile, "--format", "goose", "--out", exportDir) //nolint:gosec // G204: test executes CLI
	out, err := cmdGoose.CombinedOutput()
	if err != nil {
		t.Fatalf("export goose failed: %v\nOutput: %s", err, string(out))
	}

	files, err := os.ReadDir(exportDir)
	if err != nil {
		t.Fatalf("reading export dir: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("expected exported files in %s, got 0", exportDir)
	}

	content, err := os.ReadFile(filepath.Join(exportDir, files[0].Name())) //nolint:gosec // G304: test reads generated export artifact
	if err != nil {
		t.Fatalf("reading exported file: %v", err)
	}
	if !strings.Contains(string(content), "-- +goose Up") {
		t.Fatalf("expected goose Up header, got: %s", string(content))
	}
}

func TestCLI_RedactsDSNCredentials(t *testing.T) {
	secretDSN := "postgres://admin:supersecretpassword@127.0.0.1:5432/proddb"
	cmd := exec.Command(buildBinary(t), "check", "--dsn", secretDSN, "--schema", "nonexistent.sql") //nolint:gosec // G204: test executes CLI
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "supersecretpassword") {
		t.Fatalf("found raw password in CLI output: %s", string(out))
	}
}

func TestCLI_Version(t *testing.T) {
	binPath := buildBinary(t)

	for _, flagOrCmd := range []string{"version", "--version", "-v"} {
		cmd := exec.Command(binPath, flagOrCmd) //nolint:gosec // G204: test executes CLI
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("running %s failed: %v\nOutput: %s", flagOrCmd, err, string(out))
		}
		if !strings.Contains(string(out), "grizzle") {
			t.Fatalf("expected grizzle in output, got: %s", string(out))
		}
	}
}
