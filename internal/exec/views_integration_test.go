//go:build integration

package exec_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
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
