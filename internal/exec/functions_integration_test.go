//go:build integration

package exec_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestFunctions_Lifecycle verifies function create, body-drift replace
// (CREATE OR REPLACE), signature-change drop gate, and second-sync no-op
// against real PostgreSQL.
func TestFunctions_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_fn_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	fn := func(body string) string {
		return fmt.Sprintf(`
			CREATE TABLE kv (
				k BIGINT PRIMARY KEY
			);
			CREATE FUNCTION add_one(v integer) RETURNS integer LANGUAGE plpgsql AS $fn$
			BEGIN
				RETURN %s;
			END
			$fn$;
		`, body)
	}

	newCfg := func(sql string) exec.PostgresExecConfig {
		return exec.PostgresExecConfig{
			TargetSchema:     schema,
			SchemaSQL:        sql,
			Filters:          scope.Filters{},
			Policy:           plan.DropPolicy{},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
	}

	// 1. Greenfield sync: table + function.
	if err := exec.SyncPostgres(ctx, db, newCfg(fn("v"))); err != nil {
		t.Fatalf("greenfield sync: %v", err)
	}
	var def string
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s' AND p.proname = 'add_one';`, schema,
	)).Scan(&def); err != nil {
		t.Fatalf("query function: %v", err)
	}

	// 2. Second sync: no-op.
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(fn("v")))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p.Steps)
	}

	// 3. Body drift: non-destructive CREATE OR REPLACE.
	p2, err := exec.PlanDiffPostgres(ctx, db, newCfg(fn("v + 1")))
	if err != nil {
		t.Fatalf("drift plan: %v", err)
	}
	if len(p2.Steps) != 1 || p2.Steps[0].Type != plan.ChangeCreateFunction {
		t.Fatalf("body drift must yield single CREATE_FUNCTION, got %+v", p2.Steps)
	}
	if p2.Steps[0].Destructive {
		t.Fatalf("replace must not be destructive")
	}
	if err := exec.SyncPostgres(ctx, db, newCfg(fn("v + 1"))); err != nil {
		t.Fatalf("replace sync: %v", err)
	}
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s' AND p.proname = 'add_one';`, schema,
	)).Scan(&def); err != nil {
		t.Fatalf("query replaced function: %v", err)
	}
	if !strings.Contains(def, "v + 1") {
		t.Fatalf("replaced body not applied: %s", def)
	}

	// 4. Signature change: DROP (gated) + CREATE.
	sigCfg := newCfg(`
		CREATE TABLE kv (
			k BIGINT PRIMARY KEY
		);
		CREATE FUNCTION add_one(v bigint) RETURNS bigint LANGUAGE plpgsql AS $fn$
		BEGIN
			RETURN v + 1;
		END
		$fn$;
	`)
	p3, err := exec.PlanDiffPostgres(ctx, db, sigCfg)
	if err != nil {
		t.Fatalf("signature plan: %v", err)
	}
	var sawDrop bool
	for _, s := range p3.Steps {
		if s.Type == plan.ChangeDropFunction {
			sawDrop = true
			if !s.Destructive {
				t.Fatalf("signature DROP_FUNCTION must be destructive")
			}
		}
	}
	if !sawDrop {
		t.Fatalf("signature change must include DROP_FUNCTION, got %+v", p3.Steps)
	}
	// Default policy rejects the drop.
	if err := exec.SyncPostgres(ctx, db, sigCfg); err == nil {
		t.Fatalf("signature sync must fail with default policy (AllowFunction=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}
	// Allowed: drop + create succeeds.
	allowCfg := sigCfg
	allowCfg.Policy = plan.DropPolicy{AllowFunction: true}
	allowCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropFunction}
	if err := exec.SyncPostgres(ctx, db, allowCfg); err != nil {
		t.Fatalf("allowed signature sync: %v", err)
	}
	var retType string
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT pg_get_function_result(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s' AND p.proname = 'add_one';`, schema,
	)).Scan(&retType); err != nil {
		t.Fatalf("query re-signed function: %v", err)
	}
	if retType != "bigint" {
		t.Fatalf("expected bigint return after signature change, got %q", retType)
	}
}

// TestFunctions_LiveOnlyGatedByAllowFunction verifies removing a function
// from SchemaSQL produces a destructive DROP_FUNCTION rejected by default
// and executed only with AllowFunction + accepted hazard.
func TestFunctions_LiveOnlyGatedByAllowFunction(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_fn_live_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	withFn := `
		CREATE TABLE t (k BIGINT PRIMARY KEY);
		CREATE FUNCTION shout(s text) RETURNS text LANGUAGE sql AS $$SELECT upper(s)$$;
	`
	withoutFn := `CREATE TABLE t (k BIGINT PRIMARY KEY);`

	newCfg := func(sql string) exec.PostgresExecConfig {
		return exec.PostgresExecConfig{
			TargetSchema:     schema,
			SchemaSQL:        sql,
			Filters:          scope.Filters{},
			Policy:           plan.DropPolicy{},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
	}

	if err := exec.SyncPostgres(ctx, db, newCfg(withFn)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(withoutFn))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(p.Steps) != 1 || p.Steps[0].Type != plan.ChangeDropFunction || !p.Steps[0].Destructive {
		t.Fatalf("expected single destructive DROP_FUNCTION, got %+v", p.Steps)
	}

	if err := exec.SyncPostgres(ctx, db, newCfg(withoutFn)); err == nil {
		t.Fatalf("drop must fail with default policy (AllowFunction=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}

	allowCfg := newCfg(withoutFn)
	allowCfg.Policy = plan.DropPolicy{AllowFunction: true}
	allowCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropFunction}
	if err := exec.SyncPostgres(ctx, db, allowCfg); err != nil {
		t.Fatalf("allowed drop sync: %v", err)
	}
	var count int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s' AND p.proname = 'shout';`, schema,
	)).Scan(&count); err != nil {
		t.Fatalf("query function count: %v", err)
	}
	if count != 0 {
		t.Fatalf("function must be dropped, count = %d", count)
	}
}
