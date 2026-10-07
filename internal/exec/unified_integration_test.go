//go:build integration

package exec_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestUnifiedSchemaSQL_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	targetSchema := "test_unified_" + suffix
	roleName := "test_unified_role_" + suffix
	publicationName := "test_unified_pub_" + suffix
	eventTriggerName := "test_unified_trigger_" + suffix
	functionName := "test_unified_fn_" + suffix

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %q;", targetSchema)); err != nil {
		t.Fatalf("failed creating target schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP EVENT TRIGGER IF EXISTS %q;", eventTriggerName))
		_, _ = db.Exec(fmt.Sprintf("DROP PUBLICATION IF EXISTS %q;", publicationName))
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", targetSchema))
		_, _ = db.Exec(fmt.Sprintf("DROP ROLE IF EXISTS %q;", roleName))
	}()

	schemaSQL := fmt.Sprintf(`
		CREATE TABLE docs (id bigint PRIMARY KEY, body text NOT NULL);
		CREATE FUNCTION %s() RETURNS event_trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE NOTICE 'ddl event: %% %%', TG_EVENT, TG_TAG;
		END;
		$$;
		CREATE ROLE %s;
		GRANT SELECT ON docs TO %s;
		CREATE PUBLICATION %s FOR TABLE docs;
		CREATE EVENT TRIGGER %s ON ddl_command_end EXECUTE FUNCTION %s();
		ALTER EVENT TRIGGER %s DISABLE;
	`, functionName, roleName, roleName, publicationName, eventTriggerName, functionName, eventTriggerName)

	cfg := exec.PostgresExecConfig{
		TargetSchema: targetSchema,
		SchemaSQL:    schemaSQL,
		Filters:      scope.Filters{},
		Policy:       plan.DropPolicy{},
		CatalogSQL: fmt.Sprintf(`
			CREATE PUBLICATION %s FOR TABLE docs;
			ALTER PUBLICATION %s SET (publish = 'insert');
			CREATE EVENT TRIGGER %s ON ddl_command_end EXECUTE FUNCTION %s();
			ALTER EVENT TRIGGER %s DISABLE;
		`, publicationName, publicationName, eventTriggerName, functionName, eventTriggerName),
		LockTimeout:      5 * time.Second,
		StatementTimeout: 30 * time.Second,
	}

	if err := exec.SyncPostgres(ctx, db, cfg); err != nil {
		t.Fatalf("unified SchemaSQL sync failed: %v", err)
	}

	var tableExists, roleExists, publicationExists, triggerExists int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'docs';`, targetSchema).Scan(&tableExists); err != nil {
		t.Fatalf("querying table: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pg_roles WHERE rolname = $1;`, roleName).Scan(&roleExists); err != nil {
		t.Fatalf("querying role: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pg_publication WHERE pubname = $1;`, publicationName).Scan(&publicationExists); err != nil {
		t.Fatalf("querying publication: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pg_event_trigger WHERE evtname = $1;`, eventTriggerName).Scan(&triggerExists); err != nil {
		t.Fatalf("querying event trigger: %v", err)
	}
	if tableExists != 1 || roleExists != 1 || publicationExists != 1 || triggerExists != 1 {
		t.Fatalf("unified objects missing: table=%d role=%d publication=%d event_trigger=%d",
			tableExists, roleExists, publicationExists, triggerExists)
	}
	var pubAllTables bool
	if err := db.QueryRow(`SELECT p.puballtables FROM pg_publication p WHERE p.pubname = $1;`, publicationName).Scan(&pubAllTables); err != nil {
		t.Fatalf("querying publication membership mode: %v", err)
	}
	if pubAllTables {
		t.Fatal("CatalogSQL CREATE must overlay the SchemaSQL publication")
	}
	var publish string
	if err := db.QueryRow(`SELECT p.pubinsert::text || p.pubupdate::text || p.pubdelete::text || p.pubtruncate::text FROM pg_publication p WHERE p.pubname = $1;`, publicationName).Scan(&publish); err != nil {
		t.Fatalf("querying publication publish flags: %v", err)
	}
	if publish != "truefalsefalsefalse" {
		t.Fatalf("CatalogSQL ALTER must apply after its overlay CREATE, got %s", publish)
	}
	var triggerEnabled bool
	if err := db.QueryRow(`SELECT e.evtenabled = 'O' FROM pg_event_trigger e WHERE e.evtname = $1;`, eventTriggerName).Scan(&triggerEnabled); err != nil {
		t.Fatalf("querying event trigger enabled state: %v", err)
	}
	if triggerEnabled {
		t.Fatal("disabled event trigger must remain disabled after CREATE and overlay ALTER")
	}

	unifiedPlan, err := exec.PlanDiffPostgres(ctx, db, cfg)
	if err != nil {
		t.Fatalf("second unified plan failed: %v", err)
	}
	if len(unifiedPlan.Steps) != 0 {
		t.Fatalf("second unified plan must be a no-op, got %+v", unifiedPlan.Steps)
	}

	// Definition replacement is a destructive DROP+CREATE and must preserve
	// the disabled desired state and managed marker.
	replacementFunction := functionName + "_v2"
	replacementCfg := cfg
	replacementCfg.SchemaSQL = schemaSQL + fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS event_trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE NOTICE 'replacement';
		END;
		$$;
	`, replacementFunction)
	replacementCfg.CatalogSQL = fmt.Sprintf(`
		CREATE PUBLICATION %s FOR TABLE docs;
		ALTER PUBLICATION %s SET (publish = 'insert');
		CREATE EVENT TRIGGER %s ON ddl_command_end EXECUTE FUNCTION %s();
		ALTER EVENT TRIGGER %s DISABLE;
	`, publicationName, publicationName, eventTriggerName, replacementFunction, eventTriggerName)
	replacementPlan, err := exec.PlanDiffPostgres(ctx, db, replacementCfg)
	if err != nil {
		t.Fatalf("definition replacement plan failed: %v", err)
	}
	var replacementDrop, replacementCreate bool
	for _, step := range replacementPlan.Steps {
		switch step.Type {
		case plan.ChangeDropEventTrigger:
			replacementDrop = true
			if !step.Destructive {
				t.Fatalf("event-trigger definition replacement drop must be destructive: %+v", step)
			}
		case plan.ChangeCreateEventTrigger:
			replacementCreate = true
		}
	}
	if !replacementDrop || !replacementCreate {
		t.Fatalf("event-trigger definition drift must plan DROP+CREATE: %+v", replacementPlan.Steps)
	}
	replacementCfg.Policy = plan.DropPolicy{AllowDropEventTrigger: true}
	replacementCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropEventTrigger}
	if err := exec.SyncPostgres(ctx, db, replacementCfg); err != nil {
		t.Fatalf("definition replacement sync failed: %v", err)
	}
	var replacementEnabled bool
	var replacementMarker string
	if err := db.QueryRow(`SELECT e.evtenabled <> 'D', COALESCE(obj_description(e.oid, 'pg_event_trigger'), '') FROM pg_event_trigger e JOIN pg_proc p ON p.oid = e.evtfoid WHERE e.evtname = $1 AND p.proname = $2;`, eventTriggerName, replacementFunction).Scan(&replacementEnabled, &replacementMarker); err != nil {
		t.Fatalf("query replaced event trigger: %v", err)
	}
	if replacementEnabled || replacementMarker != "grizzle-managed" {
		t.Fatalf("replaced event trigger must be disabled and marker-stamped: enabled=%v marker=%q", replacementEnabled, replacementMarker)
	}
	replacementPlan, err = exec.PlanDiffPostgres(ctx, db, replacementCfg)
	if err != nil {
		t.Fatalf("second replacement plan failed: %v", err)
	}
	if len(replacementPlan.Steps) != 0 {
		t.Fatalf("second replacement sync must be a no-op, got %+v", replacementPlan.Steps)
	}
}
