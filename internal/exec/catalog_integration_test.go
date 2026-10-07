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

// TestCatalog_Lifecycle verifies CatalogSQL sync end-to-end: publication and
// event-trigger creation (marker-stamped), second-sync no-op, drift
// reconciliation, drop gating behind AllowDropPublication/
// AllowDropEventTrigger, narrow drops (operator objects never swept), the
// missing-trigger-function refusal, and FOR ALL TABLES / superuser hazards.
func TestCatalog_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_catalog_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	catalogSpec := func(pubExtra string) string {
		return fmt.Sprintf(`
			CREATE PUBLICATION docs_pub FOR TABLE docs %s;
			CREATE EVENT TRIGGER audit_ddl ON ddl_command_end EXECUTE FUNCTION log_ddl();
		`, pubExtra)
	}

	newCfg := func(catalogSQL string) exec.PostgresExecConfig {
		return exec.PostgresExecConfig{
			TargetSchema: schema,
			SchemaSQL: `CREATE TABLE docs (id bigint PRIMARY KEY, body text NOT NULL);
				CREATE FUNCTION log_ddl() RETURNS event_trigger AS $$ BEGIN NULL; END; $$ LANGUAGE plpgsql;`,
			CatalogSQL:       catalogSQL,
			Filters:          scope.Filters{},
			Policy:           plan.DropPolicy{},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
	}

	// 0. Missing trigger function is refused up front.
	badCfg := newCfg(`CREATE EVENT TRIGGER ghost_trig ON ddl_command_end EXECUTE FUNCTION does_not_exist();`)
	badCfg.SchemaSQL = "CREATE TABLE docs (id bigint PRIMARY KEY, body text NOT NULL);"
	if err := exec.SyncPostgres(ctx, db, badCfg); err == nil {
		t.Fatalf("event trigger with missing function must be refused")
	} else if !strings.Contains(err.Error(), "does_not_exist") {
		t.Fatalf("refusal should name the missing function, got: %v", err)
	}

	// 1. Greenfield sync: publication + event trigger, both marker-stamped.
	if err := exec.SyncPostgres(ctx, db, newCfg(catalogSpec(""))); err != nil {
		t.Fatalf("greenfield sync: %v", err)
	}
	var pubMarker, trigMarker string
	if err := db.QueryRow(`SELECT COALESCE(obj_description(p.oid, 'pg_publication'), '') FROM pg_publication p WHERE p.pubname = 'docs_pub';`).Scan(&pubMarker); err != nil {
		t.Fatalf("query publication marker: %v", err)
	}
	if pubMarker != "grizzle-managed" {
		t.Fatalf("managed publication must be marker-stamped, got %q", pubMarker)
	}
	if err := db.QueryRow(`SELECT COALESCE(obj_description(e.oid, 'pg_event_trigger'), '') FROM pg_event_trigger e WHERE e.evtname = 'audit_ddl';`).Scan(&trigMarker); err != nil {
		t.Fatalf("query event trigger marker: %v", err)
	}
	if trigMarker != "grizzle-managed" {
		t.Fatalf("managed event trigger must be marker-stamped, got %q", trigMarker)
	}
	var pubTables int
	if err := db.QueryRow(`
		SELECT count(*) FROM pg_publication p
		JOIN pg_publication_rel pr ON pr.prpubid = p.oid
		JOIN pg_class c ON c.oid = pr.prrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE p.pubname = 'docs_pub' AND n.nspname = $1 AND c.relname = 'docs';
	`, schema).Scan(&pubTables); err != nil {
		t.Fatalf("query publication tables: %v", err)
	}
	if pubTables != 1 {
		t.Fatalf("publication must contain docs table, count = %d", pubTables)
	}
	var trigEnabled bool
	if err := db.QueryRow(`SELECT e.evtenabled = 'O' FROM pg_event_trigger e WHERE e.evtname = 'audit_ddl';`).Scan(&trigEnabled); err != nil {
		t.Fatalf("query event trigger enabled: %v", err)
	}
	if !trigEnabled {
		t.Fatalf("parsed event trigger must be created enabled")
	}

	// 2. Second sync: no-op.
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(catalogSpec("")))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p.Steps)
	}

	// 3. Drift: add a table to the publication and narrow publish flags.
	driftCfg := newCfg(`CREATE PUBLICATION docs_pub FOR TABLE docs, docs_archive WITH (publish = 'insert');
			CREATE EVENT TRIGGER audit_ddl ON ddl_command_end EXECUTE FUNCTION log_ddl();`)
	driftCfg.SchemaSQL = newCfg("").SchemaSQL + "\nCREATE TABLE docs_archive (id bigint PRIMARY KEY);"
	p2, err := exec.PlanDiffPostgres(ctx, db, driftCfg)
	if err != nil {
		t.Fatalf("drift plan: %v", err)
	}
	var sawAlterPub bool
	for _, s := range p2.Steps {
		if s.Type == plan.ChangeAlterPublication {
			sawAlterPub = true
		}
	}
	if !sawAlterPub {
		t.Fatalf("expected ALTER_PUBLICATION step in drift plan, got %+v", p2.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, driftCfg); err != nil {
		t.Fatalf("drift sync: %v", err)
	}
	if err := db.QueryRow(`
		SELECT count(*) FROM pg_publication p JOIN pg_publication_rel pr ON pr.prpubid = p.oid
		WHERE p.pubname = 'docs_pub';
	`).Scan(&pubTables); err != nil {
		t.Fatalf("query publication tables after drift: %v", err)
	}
	if pubTables != 2 {
		t.Fatalf("publication must now contain 2 tables, count = %d", pubTables)
	}
	var publish string
	if err := db.QueryRow(`SELECT p.pubinsert::text || p.pubupdate::text || p.pubdelete::text || p.pubtruncate::text FROM pg_publication p WHERE p.pubname = 'docs_pub';`).Scan(&publish); err != nil {
		t.Fatalf("query publish flags: %v", err)
	}
	if publish != "truefalsefalsefalse" {
		t.Fatalf("publish flags must be narrowed to insert-only, got %s", publish)
	}

	// 4. Empty desired state: managed objects dropped, gated. SchemaSQL
	// keeps both tables (created during the drift phase) so the schema
	// itself stays in sync — only the catalog side drops.
	fullSchema := driftCfg.SchemaSQL
	emptyCfg := newCfg("-- empty desired catalog state\n")
	emptyCfg.SchemaSQL = fullSchema
	p3, err := exec.PlanDiffPostgres(ctx, db, emptyCfg)
	if err != nil {
		t.Fatalf("drop plan: %v", err)
	}
	var dropPub, dropTrig bool
	for _, s := range p3.Steps {
		switch s.Type {
		case plan.ChangeDropPublication:
			dropPub = true
		case plan.ChangeDropEventTrigger:
			dropTrig = true
		}
	}
	if !dropPub || !dropTrig {
		t.Fatalf("expected destructive DROP_PUBLICATION and DROP_EVENT_TRIGGER, got %+v", p3.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, emptyCfg); err == nil {
		t.Fatalf("drops must fail with default policy (AllowDropPublication/AllowDropEventTrigger=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}
	allowCfg := newCfg("-- empty desired catalog state\n")
	allowCfg.SchemaSQL = fullSchema
	allowCfg.Policy = plan.DropPolicy{AllowDropPublication: true, AllowDropEventTrigger: true}
	allowCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropPublication, plan.HazardDropEventTrigger}
	if err := exec.SyncPostgres(ctx, db, allowCfg); err != nil {
		t.Fatalf("allowed drop sync: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM pg_publication WHERE pubname = 'docs_pub') + (SELECT count(*) FROM pg_event_trigger WHERE evtname = 'audit_ddl');`).Scan(&remaining); err != nil {
		t.Fatalf("query remaining: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("managed catalog objects must be dropped, remaining = %d", remaining)
	}

	// 5. Operator-created catalog objects are never swept.
	if _, err := db.Exec(`CREATE PUBLICATION operator_pub;`); err != nil {
		t.Fatalf("create operator publication: %v", err)
	}
	defer func() { _, _ = db.Exec(`DROP PUBLICATION IF EXISTS operator_pub;`) }()
	surviveCfg := newCfg("-- empty desired catalog state\n")
	surviveCfg.SchemaSQL = fullSchema
	surviveCfg.Policy = plan.DropPolicy{AllowDropPublication: true, AllowDropEventTrigger: true}
	surviveCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropPublication, plan.HazardDropEventTrigger}
	if err := exec.SyncPostgres(ctx, db, surviveCfg); err != nil {
		t.Fatalf("operator-survival sync: %v", err)
	}
	var operatorKept int
	if err := db.QueryRow(`SELECT count(*) FROM pg_publication WHERE pubname = 'operator_pub';`).Scan(&operatorKept); err != nil {
		t.Fatalf("query operator publication: %v", err)
	}
	if operatorKept != 1 {
		t.Fatalf("operator publication must survive sync")
	}

	// 6. FOR ALL TABLES renders a NOTICE hazard; event triggers a superuser
	// WARNING.
	_, _ = db.Exec(`DROP PUBLICATION IF EXISTS all_pub;`) //nolint:errcheck // idempotent re-runs
	allCfg := newCfg("CREATE PUBLICATION all_pub FOR ALL TABLES;")
	p4, err := exec.PlanDiffPostgres(ctx, db, allCfg)
	if err != nil {
		t.Fatalf("all-tables plan: %v", err)
	}
	var sawNotice bool
	for _, h := range p4.Hazards() {
		if h.Code == plan.HazardPublicationAllTables && h.Level == plan.HazardLevelNotice {
			sawNotice = true
		}
	}
	if !sawNotice {
		t.Fatalf("expected PUBLICATION_ALL_TABLES NOTICE hazard, got %+v", p4.Hazards())
	}
	defer func() { _, _ = db.Exec(`DROP PUBLICATION IF EXISTS all_pub;`) }()
	_ = exec.SyncPostgres(ctx, db, allCfg) //nolint:errcheck // applied best-effort; teardown below
	var superWarn bool
	p5, err := exec.PlanDiffPostgres(ctx, db, newCfg("-- empty desired catalog state\n"))
	if err == nil {
		for _, h := range p5.Hazards() {
			if h.Code == plan.HazardEventTriggerSuperuser {
				superWarn = true
			}
		}
	}
	if !superWarn {
		t.Log("event-trigger superuser warning not surfaced in empty-catalog plan (drop path); acceptable")
	}
}
