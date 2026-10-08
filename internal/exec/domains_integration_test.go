//go:build integration

package exec_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestDomains_Lifecycle verifies domains are managed end-to-end: greenfield
// create with a CHECK constraint, second-sync no-op, constraint add/drop via
// ALTER DOMAIN, base-type drift DROP+CREATE, and live-only drop behind
// AllowDomain.
func TestDomains_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_dom_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	domainSQL := func(checkExpr string) string {
		return fmt.Sprintf(`
			CREATE DOMAIN positive_int AS integer DEFAULT 0 CHECK (%s);
			CREATE TABLE ledger (
				id BIGINT PRIMARY KEY,
				amount positive_int NOT NULL
			);
		`, checkExpr)
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

	// 1. Greenfield sync: domain + table using it.
	if err := exec.SyncPostgres(ctx, db, newCfg(domainSQL("VALUE > 0"))); err != nil {
		t.Fatalf("greenfield sync: %v", err)
	}
	var domainOID, conOID int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT t.oid, c.oid FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace LEFT JOIN pg_constraint c ON c.contypid = t.oid WHERE n.nspname = '%s' AND t.typname = 'positive_int';`, schema,
	)).Scan(&domainOID, &conOID); err != nil {
		t.Fatalf("query domain: %v", err)
	}
	if conOID == 0 {
		t.Fatalf("expected domain CHECK constraint to be created")
	}

	// 2. Second sync: no-op (round-trip introspection must be stable).
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(domainSQL("VALUE > 0")))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p.Steps)
	}

	// 3. Constraint redefinition: DROP CONSTRAINT then ADD CONSTRAINT.
	p2, err := exec.PlanDiffPostgres(ctx, db, newCfg(domainSQL("VALUE >= 0")))
	if err != nil {
		t.Fatalf("drift plan: %v", err)
	}
	if len(p2.Steps) != 2 ||
		p2.Steps[0].Type != plan.ChangeDropDomainConstraint ||
		p2.Steps[1].Type != plan.ChangeAlterDomain {
		t.Fatalf("expected DROP_DOMAIN_CONSTRAINT then ALTER_DOMAIN, got %+v", p2.Steps)
	}
	if !p2.Steps[0].Destructive {
		t.Fatalf("constraint drop must be destructive")
	}
	// Default policy denies the drop: sync must fail...
	if err := exec.SyncPostgres(ctx, db, newCfg(domainSQL("VALUE >= 0"))); err == nil {
		t.Fatalf("constraint drop must fail with default policy (AllowDomain=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}
	// ...and succeed with the gate open.
	allowDrift := newCfg(domainSQL("VALUE >= 0"))
	allowDrift.Policy = plan.DropPolicy{AllowDomain: true}
	allowDrift.AcceptHazards = []plan.HazardCode{plan.HazardDropDomain}
	if err := exec.SyncPostgres(ctx, db, allowDrift); err != nil {
		t.Fatalf("allowed drift sync: %v", err)
	}
	var conDef string
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_type t ON t.oid = c.contypid JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname = '%s' AND t.typname = 'positive_int';`, schema,
	)).Scan(&conDef); err != nil {
		t.Fatalf("query replaced constraint: %v", err)
	}
	if conDef != "CHECK ((VALUE >= 0))" {
		t.Fatalf("replaced constraint not applied: %q", conDef)
	}

	// 4. Base-type drift: DROP + CREATE (ALTER DOMAIN cannot retype).
	retypeSQL := fmt.Sprintf(`
			CREATE DOMAIN positive_int AS bigint DEFAULT 0 CHECK (VALUE >= 0);
			CREATE TABLE ledger (
				id BIGINT PRIMARY KEY,
				amount positive_int NOT NULL
			);
		`)
	p3, err := exec.PlanDiffPostgres(ctx, db, newCfg(retypeSQL))
	if err != nil {
		t.Fatalf("retype plan: %v", err)
	}
	if len(p3.Steps) != 2 || p3.Steps[0].Type != plan.ChangeDropDomainRetype || p3.Steps[1].Type != plan.ChangeCreateDomain {
		t.Fatalf("expected DROP_DOMAIN_RETYPE + CREATE_DOMAIN for retype, got %+v", p3.Steps)
	}
	// The live table still uses the domain, so the drop must fail at apply
	// time even with the gate open (no implicit CASCADE).
	retyping := newCfg(retypeSQL)
	retyping.Policy = plan.DropPolicy{AllowDomain: true}
	retyping.AcceptHazards = []plan.HazardCode{plan.HazardDropDomain}
	if err := exec.SyncPostgres(ctx, db, retyping); err == nil {
		t.Fatalf("retype with dependent columns must fail without CASCADE")
	}

	// 5. Live-only domain removal: destructive drop behind AllowDomain. The
	// dependent table must go first (table removed from SchemaSQL too).
	withoutDomain := `
			CREATE TABLE ledger2 (
				id BIGINT PRIMARY KEY,
				amount integer NOT NULL
			);
		`
	p4, err := exec.PlanDiffPostgres(ctx, db, newCfg(withoutDomain))
	if err != nil {
		t.Fatalf("live-only plan: %v", err)
	}
	var sawDropDomain bool
	for _, s := range p4.Steps {
		if s.Type == plan.ChangeDropDomain {
			sawDropDomain = true
			if !s.Destructive {
				t.Fatalf("domain drop must be destructive: %+v", s)
			}
		}
	}
	if !sawDropDomain {
		t.Fatalf("expected DROP_DOMAIN in plan, got %+v", p4.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, newCfg(withoutDomain)); err == nil {
		t.Fatalf("drop must fail with default policy (AllowDomain=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}
	allowDrop := newCfg(withoutDomain)
	allowDrop.Policy = plan.DropPolicy{AllowTable: true, AllowDomain: true}
	allowDrop.AcceptHazards = []plan.HazardCode{plan.HazardDropTable, plan.HazardDropDomain}
	if err := exec.SyncPostgres(ctx, db, allowDrop); err != nil {
		t.Fatalf("allowed drop sync: %v", err)
	}
	var count int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname = '%s' AND t.typname = 'positive_int';`, schema,
	)).Scan(&count); err != nil {
		t.Fatalf("query domain count: %v", err)
	}
	if count != 0 {
		t.Fatalf("domain must be dropped, count = %d", count)
	}
}
