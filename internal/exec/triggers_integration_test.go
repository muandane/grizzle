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

// TestTriggers_Lifecycle verifies trigger create, definition-drift replace,
// and second-sync no-op against real PostgreSQL.
func TestTriggers_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_trg_%d", time.Now().UnixNano())
	if _, err := db.Exec(`CREATE SCHEMA ` + schema + `;`); err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE;`) }()

	sql := func(triggerEvent string) string {
		return `
			CREATE TABLE docs (
				id BIGINT PRIMARY KEY,
				payload TEXT
			);
			CREATE FUNCTION trg_noop() RETURNS trigger LANGUAGE plpgsql AS $fn$
			BEGIN
				RETURN NEW;
			END
			$fn$;
			CREATE TRIGGER trg_guard BEFORE ` + triggerEvent + ` ON docs FOR EACH ROW EXECUTE FUNCTION trg_noop();
		`
	}

	newCfg := func(s string) exec.PostgresExecConfig {
		return exec.PostgresExecConfig{
			TargetSchema:     schema,
			SchemaSQL:        s,
			Filters:          scope.Filters{},
			Policy:           plan.DropPolicy{},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
	}

	// 1. Greenfield sync: table + function + trigger.
	if err := exec.SyncPostgres(ctx, db, newCfg(sql("INSERT"))); err != nil {
		t.Fatalf("greenfield sync: %v", err)
	}
	var trgCount int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = '%s' AND c.relname = 'docs' AND t.tgname = 'trg_guard';`, schema,
	)).Scan(&trgCount); err != nil {
		t.Fatalf("query trigger: %v", err)
	}
	if trgCount != 1 {
		t.Fatalf("expected trg_guard, count = %d", trgCount)
	}

	// 2. Second sync: no-op.
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(sql("INSERT")))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p.Steps)
	}

	// 3. Definition drift (INSERT -> UPDATE): DROP + CREATE.
	p2, err := exec.PlanDiffPostgres(ctx, db, newCfg(sql("UPDATE")))
	if err != nil {
		t.Fatalf("drift plan: %v", err)
	}
	var sawDrop, sawCreate bool
	for _, s := range p2.Steps {
		switch s.Type {
		case plan.ChangeDropTrigger:
			sawDrop = true
			if !s.Destructive {
				t.Errorf("DROP_TRIGGER must be destructive")
			}
		case plan.ChangeCreateTrigger:
			sawCreate = true
		}
	}
	if !sawDrop || !sawCreate {
		t.Fatalf("drift must DROP+CREATE trigger, got %+v", p2.Steps)
	}
	replaceCfg := newCfg(sql("UPDATE"))
	replaceCfg.Policy = plan.DropPolicy{AllowTrigger: true}
	replaceCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropTrigger}
	if err := exec.SyncPostgres(ctx, db, replaceCfg); err != nil {
		t.Fatalf("replace sync: %v", err)
	}
}

// TestTriggers_ColumnDropBlockedBySurvivingTrigger verifies that dropping a
// column referenced by a surviving managed trigger is blocked until the
// UNMANAGED_DEPENDENCY hazard is accepted.
func TestTriggers_ColumnDropBlockedBySurvivingTrigger(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_trgcol_%d", time.Now().UnixNano())
	if _, err := db.Exec(`CREATE SCHEMA ` + schema + `;`); err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE;`) }()

	withEmail := `
		CREATE TABLE docs (
			id BIGINT PRIMARY KEY,
			email TEXT,
			payload TEXT
		);
		CREATE FUNCTION trg_noop() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			NEW.payload := lower(NEW.email);
			RETURN NEW;
		END
		$fn$;
		CREATE TRIGGER trg_guard BEFORE INSERT ON docs FOR EACH ROW EXECUTE FUNCTION trg_noop();
	`
	withoutEmail := `
		CREATE TABLE docs (
			id BIGINT PRIMARY KEY,
			payload TEXT
		);
		CREATE FUNCTION trg_noop() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			NEW.payload := lower(NEW.email);
			RETURN NEW;
		END
		$fn$;
		CREATE TRIGGER trg_guard BEFORE INSERT ON docs FOR EACH ROW EXECUTE FUNCTION trg_noop();
	`

	newCfg := func(s string) exec.PostgresExecConfig {
		return exec.PostgresExecConfig{
			TargetSchema:     schema,
			SchemaSQL:        s,
			Filters:          scope.Filters{},
			Policy:           plan.DropPolicy{},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
	}

	if err := exec.SyncPostgres(ctx, db, newCfg(withEmail)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// Plan: DROP_COLUMN with surviving-trigger dependency hazard.
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(withoutEmail))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var hasHazard bool
	for _, h := range p.Hazards() {
		if h.Code == plan.HazardUnmanagedDependency {
			hasHazard = true
		}
	}
	if !hasHazard {
		t.Fatalf("expected UNMANAGED_DEPENDENCY hazard for surviving trigger, got %+v", p.Hazards())
	}

	// Sync without acceptance: blocked.
	if err := exec.SyncPostgres(ctx, db, newCfg(withoutEmail)); err == nil {
		t.Fatalf("column drop with surviving trigger must be blocked")
	} else if err.Error() == "" {
		t.Fatalf("expected error")
	}

	// Accepting the hazard proceeds (destructive column drop still requires AllowDropColumn).
	acceptCfg := newCfg(withoutEmail)
	acceptCfg.Policy = plan.DropPolicy{AllowColumn: true}
	acceptCfg.AcceptHazards = []plan.HazardCode{plan.HazardUnmanagedDependency, plan.HazardDropColumn}
	if err := exec.SyncPostgres(ctx, db, acceptCfg); err != nil {
		t.Fatalf("accepted sync: %v", err)
	}
	var colCount int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM information_schema.columns WHERE table_schema = '%s' AND table_name = 'docs' AND column_name = 'email';`, schema,
	)).Scan(&colCount); err != nil {
		t.Fatalf("query column: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("expected email column dropped, count = %d", colCount)
	}
}
