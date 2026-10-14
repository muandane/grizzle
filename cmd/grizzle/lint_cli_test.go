package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCLILint_StandardProfile(t *testing.T) {
	tmpDir := t.TempDir()
	schemaPath := filepath.Join(tmpDir, "schema.sql")
	schemaSQL := `
CREATE TABLE users (
    id BIGINT PRIMARY KEY,
    email VARCHAR(255) NOT NULL
);
`
	if err := os.WriteFile(schemaPath, []byte(schemaSQL), 0600); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	exitCode := runLint(context.Background(), "", "", schemaPath, "text", "standard", false, "")
	if exitCode != 0 {
		t.Errorf("expected exit code 0 for valid schema, got %d", exitCode)
	}
}

func TestCLILint_StrictProfile(t *testing.T) {
	tmpDir := t.TempDir()
	schemaPath := filepath.Join(tmpDir, "schema.sql")
	// Schema with boolean column lacking default (triggers L044 in strict profile)
	schemaSQL := `
CREATE TABLE users (
    id BIGINT PRIMARY KEY,
    is_active BOOLEAN
);
`
	if err := os.WriteFile(schemaPath, []byte(schemaSQL), 0600); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	// Standard profile ignores L044 (warning) and passes without failOnWarning
	codeStd := runLint(context.Background(), "", "", schemaPath, "text", "standard", false, "")
	if codeStd != 0 {
		t.Errorf("expected standard profile to pass, got %d", codeStd)
	}

	// Strict profile reports L044 warning; with failOnWarning=true it must return 1
	codeStrict := runLint(context.Background(), "", "", schemaPath, "text", "strict", true, "")
	if codeStrict != 1 {
		t.Errorf("expected strict profile with failOnWarning to return 1, got %d", codeStrict)
	}
}

func TestCLILint_ConfigDisabledRules(t *testing.T) {
	tmpDir := t.TempDir()
	schemaPath := filepath.Join(tmpDir, "schema.sql")
	confPath := filepath.Join(tmpDir, "grizzle.yaml")

	// Table has no primary key (normally triggers L001 error)
	schemaSQL := `
CREATE TABLE events (
    name TEXT
);
`
	if err := os.WriteFile(schemaPath, []byte(schemaSQL), 0600); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	// Config disables L001
	yamlConf := `
lint:
  disabled_rules:
    - L001
`
	if err := os.WriteFile(confPath, []byte(yamlConf), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	exitCode := runLint(context.Background(), "", "", schemaPath, "text", "standard", false, confPath)
	if exitCode != 0 {
		t.Errorf("expected exit code 0 when L001 is disabled in config, got %d", exitCode)
	}
}
