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
	`, functionName, roleName, roleName, publicationName, eventTriggerName, functionName)

	cfg := exec.PostgresExecConfig{
		TargetSchema:     targetSchema,
		SchemaSQL:        schemaSQL,
		Filters:          scope.Filters{},
		Policy:           plan.DropPolicy{},
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

	plan, err := exec.PlanDiffPostgres(ctx, db, cfg)
	if err != nil {
		t.Fatalf("second unified plan failed: %v", err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("second unified plan must be a no-op, got %+v", plan.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, cfg); err != nil {
		t.Fatalf("second unified sync failed: %v", err)
	}
}
