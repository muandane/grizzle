//go:build integration

package exec_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestProcedures_Lifecycle verifies procedures are managed end-to-end:
// greenfield create, body-drift CREATE OR REPLACE PROCEDURE, second-sync
// no-op, and live-only drop behind the function drop gate.
func TestProcedures_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_proc_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	proc := func(body string) string {
		return fmt.Sprintf(`
			CREATE TABLE kv (
				k BIGINT PRIMARY KEY
			);
			CREATE PROCEDURE touch(id bigint) LANGUAGE plpgsql AS $p$
			BEGIN
				UPDATE kv SET k = %s WHERE k = id;
			END
			$p$;
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

	// 1. Greenfield sync: table + procedure.
	if err := exec.SyncPostgres(ctx, db, newCfg(proc("id"))); err != nil {
		t.Fatalf("greenfield sync: %v", err)
	}
	var kind string
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT p.prokind FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s' AND p.proname = 'touch';`, schema,
	)).Scan(&kind); err != nil {
		t.Fatalf("query procedure: %v", err)
	}
	if kind != "p" {
		t.Fatalf("expected prokind 'p', got %q", kind)
	}

	// 2. Second sync: no-op.
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(proc("id")))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p.Steps)
	}

	// 3. Body drift: non-destructive CREATE OR REPLACE PROCEDURE.
	p2, err := exec.PlanDiffPostgres(ctx, db, newCfg(proc("id + 1")))
	if err != nil {
		t.Fatalf("drift plan: %v", err)
	}
	if len(p2.Steps) != 1 || p2.Steps[0].Type != plan.ChangeCreateFunction {
		t.Fatalf("procedure body drift must yield single CREATE_FUNCTION, got %+v", p2.Steps)
	}
	if p2.Steps[0].Destructive {
		t.Fatalf("replace must not be destructive")
	}
	if err := exec.SyncPostgres(ctx, db, newCfg(proc("id + 1"))); err != nil {
		t.Fatalf("replace sync: %v", err)
	}
	var body string
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT p.prosrc FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s' AND p.proname = 'touch';`, schema,
	)).Scan(&body); err != nil {
		t.Fatalf("query replaced procedure: %v", err)
	}
	if !strings.Contains(body, "id + 1") {
		t.Fatalf("replaced body not applied: %q", body)
	}

	// 4. Live-only procedure removal: destructive drop behind AllowFunction.
	withoutProc := `CREATE TABLE kv (k BIGINT PRIMARY KEY);`
	p3, err := exec.PlanDiffPostgres(ctx, db, newCfg(withoutProc))
	if err != nil {
		t.Fatalf("live-only plan: %v", err)
	}
	if len(p3.Steps) != 1 || p3.Steps[0].Type != plan.ChangeDropFunction || !p3.Steps[0].Destructive {
		t.Fatalf("expected single destructive DROP_FUNCTION (PROCEDURE), got %+v", p3.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, newCfg(withoutProc)); err == nil {
		t.Fatalf("drop must fail with default policy (AllowFunction=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}
	allowCfg := newCfg(withoutProc)
	allowCfg.Policy = plan.DropPolicy{AllowFunction: true}
	allowCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropFunction}
	if err := exec.SyncPostgres(ctx, db, allowCfg); err != nil {
		t.Fatalf("allowed drop sync: %v", err)
	}
	var count int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s' AND p.proname = 'touch' AND p.prokind = 'p';`, schema,
	)).Scan(&count); err != nil {
		t.Fatalf("query procedure count: %v", err)
	}
	if count != 0 {
		t.Fatalf("procedure must be dropped, count = %d", count)
	}
}

// TestAggregates_Lifecycle verifies aggregate management: greenfield create
// (CREATE AGGREGATE from SchemaSQL), second-sync no-op, body drift rendered
// as DROP + CREATE (no CREATE OR REPLACE for aggregates), and live-only
// drop behind the function drop gate.
func TestAggregates_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_agg_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	// Support functions and aggregate share one template so the live-only
	// phase reuses byte-identical function text (body whitespace is visible
	// to the routine diff and would otherwise produce replace churn).
	funcBlock := `
			CREATE FUNCTION sum2_s(v bigint, cur bigint) RETURNS bigint LANGUAGE plpgsql AS $s$
			BEGIN
				RETURN cur + v;
			END
			$s$;
			CREATE FUNCTION sum2_f(cur bigint) RETURNS bigint LANGUAGE plpgsql AS $f$
			BEGIN
				RETURN cur;
			END
			$f$;
	`
	aggBlock := `
			CREATE AGGREGATE sum2(v bigint) (
				SFUNC = sum2_s,
				STYPE = bigint,
				FINALFUNC = sum2_f,
				INITCOND = '%s'
			);
	`
	agg := func(initcond string) string {
		return funcBlock + fmt.Sprintf(aggBlock, initcond)
	}
	withoutAgg := funcBlock

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

	// 1. Greenfield sync: aggregate + support functions.
	if err := exec.SyncPostgres(ctx, db, newCfg(agg("0"))); err != nil {
		t.Fatalf("greenfield sync: %v", err)
	}
	var aggCount int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace JOIN pg_aggregate a ON a.aggfnoid = p.oid WHERE n.nspname = '%s' AND p.proname = 'sum2' AND a.aggkind = 'n';`, schema,
	)).Scan(&aggCount); err != nil {
		t.Fatalf("query aggregate: %v", err)
	}
	if aggCount != 1 {
		t.Fatalf("expected managed aggregate sum2, count = %d", aggCount)
	}

	// 2. Second sync: no-op (reconstruction must be stable).
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(agg("0")))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p.Steps)
	}

	// 3. Aggregate drift: DROP + CREATE (aggregates cannot be replaced in
	// place). Only INITCOND changes, so support functions are untouched.
	driftCfg := newCfg(agg("1"))
	p2, err := exec.PlanDiffPostgres(ctx, db, driftCfg)
	if err != nil {
		t.Fatalf("drift plan: %v", err)
	}
	if len(p2.Steps) != 2 {
		t.Fatalf("expected DROP+CREATE for aggregate drift, got %+v", p2.Steps)
	}
	if p2.Steps[0].Type != plan.ChangeDropAggregate || p2.Steps[1].Type != plan.ChangeCreateAggregate {
		t.Fatalf("aggregate drift must be DROP_AGGREGATE then CREATE_AGGREGATE, got %+v", p2.Steps)
	}
	if !p2.Steps[0].Destructive {
		t.Fatalf("aggregate drop must be destructive")
	}
	// Aggregate replacement includes a destructive drop: requires the gate.
	driftAllowCfg := driftCfg
	driftAllowCfg.Policy = plan.DropPolicy{AllowFunction: true}
	driftAllowCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropFunction}
	if err := exec.SyncPostgres(ctx, db, driftAllowCfg); err != nil {
		t.Fatalf("drift sync: %v", err)
	}
	var initcond string
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT a.agginitval FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace JOIN pg_aggregate a ON a.aggfnoid = p.oid WHERE n.nspname = '%s' AND p.proname = 'sum2';`, schema,
	)).Scan(&initcond); err != nil {
		t.Fatalf("query replaced aggregate: %v", err)
	}
	if initcond != "1" {
		t.Fatalf("replaced aggregate not applied, initcond = %q", initcond)
	}

	// 4. Live-only aggregate removal: only the aggregate is dropped; support
	// functions remain desired. DROP_AGGREGATE is destructive and gated
	// behind AllowFunction.
	p3, err := exec.PlanDiffPostgres(ctx, db, newCfg(withoutAgg))
	if err != nil {
		t.Fatalf("live-only plan: %v", err)
	}
	if len(p3.Steps) != 1 || p3.Steps[0].Type != plan.ChangeDropAggregate || !p3.Steps[0].Destructive {
		t.Fatalf("expected single destructive DROP_AGGREGATE, got %+v", p3.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, newCfg(withoutAgg)); err == nil {
		t.Fatalf("drop must fail with default policy (AllowFunction=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}
	allowCfg := newCfg(withoutAgg)
	allowCfg.Policy = plan.DropPolicy{AllowFunction: true}
	allowCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropFunction}
	if err := exec.SyncPostgres(ctx, db, allowCfg); err != nil {
		t.Fatalf("allowed drop sync: %v", err)
	}
	var dropped, funcsKept int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s' AND p.proname = 'sum2' AND p.prokind = 'a';`, schema,
	)).Scan(&dropped); err != nil {
		t.Fatalf("query aggregate count: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("aggregate must be dropped, count = %d", dropped)
	}
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s' AND p.proname LIKE 'sum2_%%';`, schema,
	)).Scan(&funcsKept); err != nil {
		t.Fatalf("query support functions: %v", err)
	}
	if funcsKept != 2 {
		t.Fatalf("support functions must remain managed, count = %d", funcsKept)
	}
}
