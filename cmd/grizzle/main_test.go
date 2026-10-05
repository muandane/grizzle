package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_HelpAndUnknown(t *testing.T) {
	// Build CLI binary for testing
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "grizzle")

	buildCmd := exec.Command("go", "build", "-o", binPath, ".") //nolint:gosec // G204: test builds newly compiled CLI binary
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed building CLI: %v\nOutput: %s", err, string(out))
	}

	// 1. Run without arguments
	cmd := exec.Command(binPath) //nolint:gosec // G204: test executes newly built CLI binary
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("expected error running without args, got success")
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Errorf("expected usage output, got: %s", string(out))
	}

	// 2. Run with unknown command
	cmdUnknown := exec.Command(binPath, "foo", "-dsn", "postgres://localhost:5432") //nolint:gosec // G204: test executes newly built CLI binary
	outUnknown, err := cmdUnknown.CombinedOutput()
	if err == nil {
		t.Errorf("expected error running unknown command, got success")
	}
	if !strings.Contains(string(outUnknown), "Unknown command") {
		t.Errorf("expected Unknown command output, got: %s", string(outUnknown))
	}

	// 3. Run without DSN
	cmdNoDSN := exec.Command(binPath, "plan") //nolint:gosec // G204: test executes newly built CLI binary
	cmdNoDSN.Env = append(os.Environ(), "DATABASE_URL=")
	outNoDSN, err := cmdNoDSN.CombinedOutput()
	if err == nil {
		t.Errorf("expected error running without DSN, got success")
	}
	if !strings.Contains(string(outNoDSN), "database DSN is required") {
		t.Errorf("expected DSN required message, got: %s", string(outNoDSN))
	}
}
