package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/muandane/grizzle/internal/config"
)

func TestConfig_YAML(t *testing.T) {
	yamlContent := `
# Project configuration
dsn: "postgres://user:pass@localhost:5432/app"
schema: "db/schema.sql"
dialect: "postgres"
target_schema: "public"
allow_drop: true
roles: "db/roles.sql"
seed: "db/seed.sql"
before_sync: "db/before.sql"
after_sync: "db/after.sql"
accept_hazards:
  - DROP_TABLE
  - DROP_COLUMN
lint:
  fail_on_warning: true
  disabled_rules:
    - L001
    - L004
`

	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "grizzle.yaml")
	if err := os.WriteFile(confPath, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("failed writing test yaml: %v", err)
	}

	cfg, err := config.Load(confPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.DSN != "postgres://user:pass@localhost:5432/app" {
		t.Errorf("expected DSN, got %s", cfg.DSN)
	}
	if cfg.Schema != "db/schema.sql" {
		t.Errorf("expected Schema, got %s", cfg.Schema)
	}
	if cfg.Dialect != "postgres" {
		t.Errorf("expected Dialect postgres, got %s", cfg.Dialect)
	}
	if cfg.TargetSchema != "public" {
		t.Errorf("expected TargetSchema public, got %s", cfg.TargetSchema)
	}
	if !cfg.AllowDrop {
		t.Errorf("expected AllowDrop true")
	}
	if cfg.BeforeSync != "db/before.sql" {
		t.Errorf("expected BeforeSync, got %s", cfg.BeforeSync)
	}
	if len(cfg.AcceptHazards) != 2 || cfg.AcceptHazards[0] != "DROP_TABLE" || cfg.AcceptHazards[1] != "DROP_COLUMN" {
		t.Errorf("unexpected AcceptHazards: %+v", cfg.AcceptHazards)
	}
	if !cfg.Lint.FailOnWarning {
		t.Errorf("expected Lint.FailOnWarning true")
	}
	if len(cfg.Lint.DisabledRules) != 2 || cfg.Lint.DisabledRules[0] != "L001" {
		t.Errorf("unexpected DisabledRules: %+v", cfg.Lint.DisabledRules)
	}
}

func TestConfig_JSON(t *testing.T) {
	jsonContent := `{
  "dsn": "sqlite:app.db",
  "schema": "schema.sql",
  "allow_drop": false,
  "accept_hazards": ["DROP_INDEX"]
}`
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "grizzle.json")
	if err := os.WriteFile(confPath, []byte(jsonContent), 0600); err != nil {
		t.Fatalf("failed writing test json: %v", err)
	}

	cfg, err := config.Load(confPath)
	if err != nil {
		t.Fatalf("Load JSON failed: %v", err)
	}

	if cfg.DSN != "sqlite:app.db" {
		t.Errorf("expected DSN sqlite:app.db, got %s", cfg.DSN)
	}
	if len(cfg.AcceptHazards) != 1 || cfg.AcceptHazards[0] != "DROP_INDEX" {
		t.Errorf("unexpected AcceptHazards: %+v", cfg.AcceptHazards)
	}
}
