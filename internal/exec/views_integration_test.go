//go:build integration

package exec_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestViews_Lifecycle verifies view create, append-columns replace
// (CREATE OR REPLACE), column-removal drop+create, matview refresh, and
// second-sync no-op against real PostgreSQL.
func TestViews_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_view_%d", time.Now().UnixNano())
	if _, err := db.Exec(`CREATE SCHEMA ` + schema + `;`); err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE;`) }()

	withBase := `
		CREATE TABLE docs (
			id BIGINT PRIMARY KEY,
			payload TEXT
		);
	`
	viewNarrow := `CREATE VIEW docs_recent AS SELECT id FROM docs WHERE id > 0;`
	viewWide := `CREATE VIEW docs_recent AS SELECT id, payload FROM docs WHERE id > 0;`
	matView := `CREATE MATERIALIZED VIEW docs_summary AS SELECT count(*) AS total FROM docs;`

	newCfg := func(extra ...string) exec.PostgresExecConfig {
		sql := withBase
		for _, e := range extra {
			sql += "\n" + e
		}
		return exec.PostgresExecConfig{
			TargetSchema:     schema,
			SchemaSQL:        sql,
			Filters:          scope.Filters{},
			Policy:           plan.DropPolicy{},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
	}

	// 1. Greenfield sync: table + view + matview.
	if err := exec.SyncPostgres(ctx, db, newCfg(viewNarrow, matView)); err != nil {
		t.Fatalf("greenfield sync: %v", err)
	}
	var matCount int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = '%s' AND c.relname = 'docs_summary' AND c.relkind = 'm';`, schema,
	)).Scan(&matCount); err != nil {
		t.Fatalf("query matview: %v", err)
	}
	if matCount != 1 {
		t.Fatalf("expected docs_summary matview, count = %d", matCount)
	}

	// 2. Second sync: no-op.
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(viewNarrow, matView))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p.Steps)
	}

	// 3. Append-only drift: CREATE OR REPLACE VIEW (non-destructive).
	p2, err := exec.PlanDiffPostgres(ctx, db, newCfg(viewWide, matView))
	if err != nil {
		t.Fatalf("append plan: %v", err)
	}
	if len(p2.Steps) != 1 || p2.Steps[0].Type != plan.ChangeCreateView || !p2.Steps[0].Replace || p2.Steps[0].Destructive {
		t.Fatalf("append-only drift must be a non-destructive replace, got %+v", p2.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, newCfg(viewWide, matView)); err != nil {
		t.Fatalf("replace sync: %v", err)
	}
	var colCount int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM information_schema.columns WHERE table_schema = '%s' AND table_name = 'docs_recent' AND column_name = 'payload';`, schema,
	)).Scan(&colCount); err != nil {
		t.Fatalf("query view column: %v", err)
	}
	if colCount != 1 {
		t.Fatalf("expected payload column on docs_recent, count = %d", colCount)
	}

	// 4. Column-removal drift: DROP + CREATE (destructive, gated).
	p3, err := exec.PlanDiffPostgres(ctx, db, newCfg(viewNarrow, matView))
	if err != nil {
		t.Fatalf("narrow plan: %v", err)
	}
	var sawDropView bool
	for _, s := range p3.Steps {
		if s.Type == plan.ChangeDropView {
			sawDropView = true
			if !s.Destructive {
				t.Errorf("DROP_VIEW must be destructive")
			}
		}
	}
	if !sawDropView {
		t.Fatalf("column-removal drift must DROP_VIEW, got %+v", p3.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, newCfg(viewNarrow, matView)); err == nil {
		t.Fatalf("DROP_VIEW must fail with default policy (AllowView=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}
	allowCfg := newCfg(viewNarrow, matView)
	allowCfg.Policy = plan.DropPolicy{AllowView: true}
	allowCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropView}
	if err := exec.SyncPostgres(ctx, db, allowCfg); err != nil {
		t.Fatalf("allowed narrow sync: %v", err)
	}

	// 5. Matview definition drift: DROP + CREATE + REFRESH.
	matDrift := `CREATE MATERIALIZED VIEW docs_summary AS SELECT count(*) AS total, max(id) AS max_id FROM docs;`
	allowCfg2 := newCfg(viewNarrow, matDrift)
	allowCfg2.Policy = plan.DropPolicy{AllowView: true}
	allowCfg2.AcceptHazards = []plan.HazardCode{plan.HazardDropView}
	p4, err := exec.PlanDiffPostgres(ctx, db, allowCfg2)
	if err != nil {
		t.Fatalf("matview plan: %v", err)
	}
	var sawRefresh bool
	for _, s := range p4.Steps {
		if s.Type == plan.ChangeRefreshMatView {
			sawRefresh = true
		}
	}
	if !sawRefresh {
		t.Fatalf("matview drift must include REFRESH, got %+v", p4.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, allowCfg2); err != nil {
		t.Fatalf("matview sync: %v", err)
	}
	var colCount2 int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = '%s' AND c.relname = 'docs_summary' AND a.attname = 'max_id' AND NOT a.attisdropped;`, schema,
	)).Scan(&colCount2); err != nil {
		t.Fatalf("query matview column: %v", err)
	}
	if colCount2 != 1 {
		t.Fatalf("expected max_id column on docs_summary, count = %d", colCount2)
	}
}

// TestViews_DAGTopologicalOrder verifies that views depending on other views
// in reverse-alphabetical order are sorted and created in topological order via pg_depend.
func TestViews_DAGTopologicalOrder(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_view_dag_%d", time.Now().UnixNano())
	if _, err := db.Exec(`CREATE SCHEMA ` + schema + `;`); err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE;`) }()

	// view_a depends on view_b (view_b must be created before view_a).
	// Alphabetically view_a < view_b.
	schemaSQL := `
		CREATE TABLE items (
			id BIGINT PRIMARY KEY
		);
		CREATE VIEW view_b AS SELECT id FROM items;
		CREATE VIEW view_a AS SELECT id FROM view_b;
	`

	cfg := exec.PostgresExecConfig{
		TargetSchema:     schema,
		SchemaSQL:        schemaSQL,
		Filters:          scope.Filters{},
		Policy:           plan.DropPolicy{},
		LockTimeout:      5 * time.Second,
		StatementTimeout: 30 * time.Second,
	}

	p, err := exec.PlanDiffPostgres(ctx, db, cfg)
	if err != nil {
		t.Fatalf("plan view DAG: %v", err)
	}

	var createViews []string
	for _, s := range p.Steps {
		if s.Type == plan.ChangeCreateView {
			createViews = append(createViews, s.Table)
		}
	}

	if len(createViews) != 2 || createViews[0] != "view_b" || createViews[1] != "view_a" {
		t.Fatalf("expected view_b before view_a in plan, got %v", createViews)
	}

	if err := exec.SyncPostgres(ctx, db, cfg); err != nil {
		t.Fatalf("sync view DAG: %v", err)
	}

	// Drop both views: referencing view_a must be dropped before referenced view_b.
	dropCfg := exec.PostgresExecConfig{
		TargetSchema:     schema,
		SchemaSQL:        `CREATE TABLE items (id BIGINT PRIMARY KEY);`,
		Filters:          scope.Filters{},
		Policy:           plan.DropPolicy{AllowView: true},
		AcceptHazards:    []plan.HazardCode{plan.HazardDropView},
		LockTimeout:      5 * time.Second,
		StatementTimeout: 30 * time.Second,
	}

	dropPlan, err := exec.PlanDiffPostgres(ctx, db, dropCfg)
	if err != nil {
		t.Fatalf("plan drop view DAG: %v", err)
	}

	var dropViews []string
	for _, s := range dropPlan.Steps {
		if s.Type == plan.ChangeDropView {
			dropViews = append(dropViews, s.Table)
		}
	}

	if len(dropViews) != 2 || dropViews[0] != "view_a" || dropViews[1] != "view_b" {
		t.Fatalf("expected view_a dropped before view_b in plan, got %v", dropViews)
	}

	if err := exec.SyncPostgres(ctx, db, dropCfg); err != nil {
		t.Fatalf("sync drop view DAG: %v", err)
	}
}

// TestViews_FunctionAndTypeDependencies verifies that views referencing functions
// and functions referencing custom types extract their dependencies from pg_depend.
func TestViews_FunctionAndTypeDependencies(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_fn_dep_%d", time.Now().UnixNano())
	if _, err := db.Exec(`CREATE SCHEMA ` + schema + `;`); err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE;`) }()

	schemaSQL := fmt.Sprintf(`
		CREATE TYPE %q.custom_pair AS (a INT, b INT);

		CREATE OR REPLACE FUNCTION %q.sum_pair(p %q.custom_pair)
		RETURNS INT LANGUAGE sql AS 'SELECT p.a + p.b';

		CREATE VIEW %q.pair_view AS
		SELECT %q.sum_pair(ROW(1, 2)::%q.custom_pair) AS total;
	`, schema, schema, schema, schema, schema, schema)

	cfg := exec.PostgresExecConfig{
		TargetSchema:     schema,
		SchemaSQL:        schemaSQL,
		Filters:          scope.Filters{},
		Policy:           plan.DropPolicy{},
		LockTimeout:      5 * time.Second,
		StatementTimeout: 30 * time.Second,
	}

	if err := exec.SyncPostgres(ctx, db, cfg); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	// Inspect the schema to verify DependsOn extraction
	sch, err := postgres.Inspect(ctx, db, schema)
	if err != nil {
		t.Fatalf("inspect failed: %v", err)
	}

	v, ok := sch.Views["pair_view"]
	if !ok {
		t.Fatalf("view pair_view not found in inspected schema")
	}
	expectedFnDep := schema + ".sum_pair"
	foundFn := false
	for _, d := range v.DependsOn {
		if d == expectedFnDep || d == "sum_pair" {
			foundFn = true
			break
		}
	}
	if !foundFn {
		t.Errorf("expected view to depend on function %s, got dependencies: %v", expectedFnDep, v.DependsOn)
	}

	foundType := false
	for _, r := range sch.Routines {
		if r.Name == "sum_pair" {
			for _, d := range r.DependsOn {
				if d == schema+".custom_pair" || d == "custom_pair" {
					foundType = true
					break
				}
			}
		}
	}
	if !foundType {
		t.Errorf("expected routine sum_pair to depend on custom_pair type, routines: %+v", sch.Routines)
	}
}
