package main

import (
	"os"
	"testing"
)

func TestExample_SQLite(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed getting wd: %v", err)
	}
	defer func() { _ = os.Chdir(origDir) }()

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed changing dir: %v", err)
	}

	// Run example main()
	main()

	// Verify app.db was created
	if _, err := os.Stat("app.db"); err != nil {
		t.Errorf("expected app.db to be created, err: %v", err)
	}
}
