//go:build integration

package exec_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestExtensions_SyncRoundTrip verifies declarative extension management:
// greenfield sync creates the extension, a second sync is a no-op, and the
// extension-provided type (citext) compiles in the shadow for column DDL.
func TestExtensions_SyncRoundTrip(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_ext_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	schemaSQL := `
		CREATE EXTENSION IF NOT EXISTS citext;

		CREATE TABLE users (
			id BIGINT PRIMARY KEY,
			email CITEXT NOT NULL
		);
	`

	newCfg := func() exec.PostgresExecConfig {
		return exec.PostgresExecConfig{
			TargetSchema:     schema,
			SchemaSQL:        schemaSQL,
			Filters:          scope.Filters{},
			Policy:           plan.DropPolicy{},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
	}

	// First sync: creates extension + table.
	cfg := newCfg()
	if err := exec.SyncPostgres(ctx, db, cfg); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	var extCount int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'citext' AND n.nspname = $1;`,
		schema,
	).Scan(&extCount); err != nil {
		t.Fatalf("query pg_extension: %v", err)
	}
	if extCount != 1 {
		t.Fatalf("expected citext installed in %s, count = %d", schema, extCount)
	}

	// Second sync: empty plan (no-op).
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg())
	if err != nil {
		t.Fatalf("plan after first sync: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %d steps: %+v", len(p.Steps), p.Steps)
	}

	// Add a new table using citext again; sync must stay clean (extension already present).
	schemaSQL += "\nCREATE TABLE emails (id BIGINT PRIMARY KEY, addr CITEXT);"
	p2, err := exec.PlanDiffPostgres(ctx, db, newCfg())
	if err != nil {
		t.Fatalf("plan after table add: %v", err)
	}
	for _, s := range p2.Steps {
		if s.Type == plan.ChangeCreateExtension {
			t.Fatalf("extension already installed; must not re-plan: %+v", s)
		}
	}
	if len(p2.Steps) != 1 || p2.Steps[0].Type != plan.ChangeCreateTable {
		t.Fatalf("expected single CREATE_TABLE step, got %+v", p2.Steps)
	}
}

// TestExtensions_HazardWarning verifies CREATE_EXTENSION carries the
// EXTENSION_PRIVILEGE warning hazard.
func TestExtensions_HazardWarning(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_exthz_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	p, err := exec.PlanDiffPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL:    `CREATE EXTENSION IF NOT EXISTS pgcrypto;`,
		Filters:      scope.Filters{},
		Policy:       plan.DropPolicy{},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	found := false
	for _, h := range p.Hazards() {
		if h.Code == plan.HazardExtensionPrivilege {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EXTENSION_PRIVILEGE hazard, got %+v", p.Hazards())
	}
}
