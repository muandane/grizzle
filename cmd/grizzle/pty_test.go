package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/mattn/go-isatty"
	_ "modernc.org/sqlite"
)

func TestInteractiveTTY_RealPTY(t *testing.T) {
	// Verify system supports PTY allocation
	ptmx, pts, err := pty.Open()
	if err != nil {
		t.Skipf("skipping real PTY test, pseudo-terminal allocation failed: %v", err)
	}
	_ = ptmx.Close()
	_ = pts.Close()

	// Setup clean SQLite test database
	tmpDir := t.TempDir()
	dbFile := filepath.Join(tmpDir, "test_pty.db")
	dsn := fmt.Sprintf("sqlite://%s", dbFile)

	// Step 1: Initial schema with table users(id, legacy_role)
	schemaV1 := filepath.Join(tmpDir, "schema_v1.sql")
	if err := os.WriteFile(schemaV1, []byte("CREATE TABLE users (id INTEGER PRIMARY KEY, legacy_role TEXT);"), 0600); err != nil {
		t.Fatalf("writing schemaV1: %v", err)
	}

	codeInit := runApply(context.Background(), dsn, "", schemaV1, "", "", "", false, nil, false, nil, false, nil)
	if codeInit != 0 {
		t.Fatalf("initial setup failed with code %d", codeInit)
	}

	// Step 2: Destructive schema V2 (drops legacy_role column -> triggers DROP_COLUMN hazard)
	schemaV2 := filepath.Join(tmpDir, "schema_v2.sql")
	if err := os.WriteFile(schemaV2, []byte("CREATE TABLE users (id INTEGER PRIMARY KEY);"), 0600); err != nil {
		t.Fatalf("writing schemaV2: %v", err)
	}

	t.Run("NonTTY_WithoutAcceptHazard_ExitsCode2", func(t *testing.T) {
		// Normal pipe is NOT a TTY
		rPipe, wPipe, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe failed: %v", err)
		}
		defer func() { _ = rPipe.Close() }()
		defer func() { _ = wPipe.Close() }()

		if isatty.IsTerminal(rPipe.Fd()) {
			t.Fatalf("os.Pipe should not be detected as terminal")
		}

		origStdin := os.Stdin
		os.Stdin = rPipe
		defer func() { os.Stdin = origStdin }()

		code := runApply(context.Background(), dsn, "", schemaV2, "", "", "", true, nil, false, nil, false, nil)
		if code != 2 {
			t.Fatalf("expected exit code 2 for non-TTY without accepted hazard, got %d", code)
		}
	})

	t.Run("RealPTY_AnswerYes_AppliesAndExitsCode0", func(t *testing.T) {
		ptmx, pts, err := pty.Open()
		if err != nil {
			t.Fatalf("pty.Open failed: %v", err)
		}
		defer func() { _ = ptmx.Close() }()
		defer func() { _ = pts.Close() }()

		if !isatty.IsTerminal(pts.Fd()) {
			t.Fatalf("pty slave must be recognized as real TTY")
		}

		origStdin := os.Stdin
		origStdout := os.Stdout
		os.Stdin = pts
		os.Stdout = pts
		defer func() {
			os.Stdin = origStdin
			os.Stdout = origStdout
		}()

		// Write 'y\n' into master PTY
		go func() {
			time.Sleep(50 * time.Millisecond)
			_, _ = ptmx.WriteString("y\n")
		}()

		code := runApply(context.Background(), dsn, "", schemaV2, "", "", "", true, nil, false, nil, false, nil)
		if code != 0 {
			t.Fatalf("expected exit code 0 when user confirms 'y' in real PTY, got %d", code)
		}
	})

	// Re-create the column for subsequent tests
	if err := os.WriteFile(schemaV1, []byte("CREATE TABLE users (id INTEGER PRIMARY KEY, legacy_role TEXT);"), 0600); err != nil {
		t.Fatalf("writing schemaV1: %v", err)
	}
	_ = runApply(context.Background(), dsn, "", schemaV1, "", "", "", false, nil, false, nil, false, nil)

	t.Run("RealPTY_AnswerNo_AbortsAndExitsCode1", func(t *testing.T) {
		ptmx, pts, err := pty.Open()
		if err != nil {
			t.Fatalf("pty.Open failed: %v", err)
		}
		defer func() { _ = ptmx.Close() }()
		defer func() { _ = pts.Close() }()

		origStdin := os.Stdin
		origStdout := os.Stdout
		os.Stdin = pts
		os.Stdout = pts
		defer func() {
			os.Stdin = origStdin
			os.Stdout = origStdout
		}()

		go func() {
			time.Sleep(50 * time.Millisecond)
			_, _ = ptmx.WriteString("N\n")
		}()

		code := runApply(context.Background(), dsn, "", schemaV2, "", "", "", true, nil, false, nil, false, nil)
		if code != 1 {
			t.Fatalf("expected exit code 1 when user aborts with 'N' in real PTY, got %d", code)
		}
	})

	t.Run("RealPTY_AnswerDetailsThenYes_ShowsSQLAndApplies", func(t *testing.T) {
		ptmx, pts, err := pty.Open()
		if err != nil {
			t.Fatalf("pty.Open failed: %v", err)
		}
		defer func() { _ = ptmx.Close() }()
		defer func() { _ = pts.Close() }()

		origStdin := os.Stdin
		origStdout := os.Stdout
		os.Stdin = pts
		os.Stdout = pts
		defer func() {
			os.Stdin = origStdin
			os.Stdout = origStdout
		}()

		// Read output asynchronously to capture output
		outCh := make(chan string, 1)
		go func() {
			var buf strings.Builder
			b := make([]byte, 1024)
			var sentDetails bool
			for {
				n, err := ptmx.Read(b)
				if n > 0 {
					buf.Write(b[:n])
					str := buf.String()
					if strings.Contains(str, "[y/N/details]:") && !sentDetails {
						sentDetails = true
						_, _ = ptmx.WriteString("details\n")
					} else if strings.Contains(str, "Full SQL statements:") && strings.Contains(str, "[y/N]:") {
						_, _ = ptmx.WriteString("y\n")
						outCh <- str
						return
					}
				}
				if err != nil {
					outCh <- buf.String()
					return
				}
			}
		}()

		code := runApply(context.Background(), dsn, "", schemaV2, "", "", "", true, nil, false, nil, false, nil)
		if code != 0 {
			t.Fatalf("expected exit code 0 when confirming after details, got %d", code)
		}

		select {
		case captured := <-outCh:
			if !strings.Contains(captured, "Full SQL statements:") {
				t.Errorf("expected captured PTY output to contain 'Full SQL statements:', got: %s", captured)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for PTY output")
		}
	})

	// Re-create the column for details->no test
	_ = runApply(context.Background(), dsn, "", schemaV1, "", "", "", false, nil, false, nil, false, nil)

	t.Run("RealPTY_AnswerDetailsThenNo_ShowsSQLAndAborts", func(t *testing.T) {
		ptmx, pts, err := pty.Open()
		if err != nil {
			t.Fatalf("pty.Open failed: %v", err)
		}
		defer func() { _ = ptmx.Close() }()
		defer func() { _ = pts.Close() }()

		origStdin := os.Stdin
		origStdout := os.Stdout
		os.Stdin = pts
		os.Stdout = pts
		defer func() {
			os.Stdin = origStdin
			os.Stdout = origStdout
		}()

		outCh := make(chan string, 1)
		go func() {
			var buf strings.Builder
			b := make([]byte, 1024)
			var sentDetails bool
			for {
				n, err := ptmx.Read(b)
				if n > 0 {
					buf.Write(b[:n])
					str := buf.String()
					if strings.Contains(str, "[y/N/details]:") && !sentDetails {
						sentDetails = true
						_, _ = ptmx.WriteString("details\n")
					} else if strings.Contains(str, "Full SQL statements:") && strings.Contains(str, "[y/N]:") {
						_, _ = ptmx.WriteString("n\n")
						outCh <- str
						return
					}
				}
				if err != nil {
					outCh <- buf.String()
					return
				}
			}
		}()

		code := runApply(context.Background(), dsn, "", schemaV2, "", "", "", true, nil, false, nil, false, nil)
		if code != 1 {
			t.Fatalf("expected exit code 1 when aborting after details, got %d", code)
		}

		select {
		case captured := <-outCh:
			if !strings.Contains(captured, "Full SQL statements:") {
				t.Errorf("expected captured PTY output to contain 'Full SQL statements:', got: %s", captured)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for PTY output")
		}
	})
}
