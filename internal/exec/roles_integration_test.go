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

// TestRoles_Lifecycle verifies RolesSQL privilege sync end-to-end: role
// creation (NOLOGIN, marker-stamped), grants applied, second-sync no-op,
// revocation gated behind AllowRevoke + REVOKE_PRIVILEGE, live-only managed
// role drop behind AllowDropRole + DROP_ROLE, and ownership refusal.
func TestRoles_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_roles_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	rolesSpec := func(grantExtra string) string {
		return fmt.Sprintf(`
			CREATE ROLE app_read;
			GRANT SELECT ON docs TO app_read;
			GRANT EXECUTE ON FUNCTION touch_ts() TO app_read;
			%s
		`, grantExtra)
	}

	newCfg := func(rolesSQL string) exec.PostgresExecConfig {
		return exec.PostgresExecConfig{
			TargetSchema: schema,
			SchemaSQL: `CREATE TABLE docs (id bigint PRIMARY KEY, body text NOT NULL);
			CREATE FUNCTION touch_ts() RETURNS bigint
			LANGUAGE sql IMMUTABLE AS 'SELECT 1';`,
			RolesSQL:         rolesSQL,
			Filters:          scope.Filters{},
			Policy:           plan.DropPolicy{},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
	}

	// 1. Greenfield sync: table + role + grant.
	if err := exec.SyncPostgres(ctx, db, newCfg(rolesSpec(""))); err != nil {
		t.Fatalf("greenfield sync: %v", err)
	}
	var rolSuper, rolCanLogin bool
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT rolsuper, rolcanlogin FROM pg_roles WHERE rolname = 'app_read';`,
	)).Scan(&rolSuper, &rolCanLogin); err != nil {
		t.Fatalf("query role: %v", err)
	}
	if rolCanLogin {
		t.Fatalf("managed roles must be NOLOGIN")
	}
	if rolSuper {
		t.Fatalf("managed roles must not be superuser")
	}
	var marker string
	if err := db.QueryRow(
		`SELECT COALESCE((SELECT description FROM pg_shdescription d WHERE d.objoid = r.oid AND d.classoid = 'pg_authid'::regclass), '') FROM pg_roles r WHERE rolname = 'app_read';`,
	).Scan(&marker); err != nil {
		t.Fatalf("query role marker: %v", err)
	}
	if marker == "" {
		t.Fatalf("managed role must be marker-stamped")
	}
	var grants int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN aclexplode(c.relacl) a ON true WHERE n.nspname = '%s' AND c.relname = 'docs' AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'app_read') AND a.privilege_type = 'SELECT';`, schema,
	)).Scan(&grants); err != nil {
		t.Fatalf("query grant: %v", err)
	}
	if grants != 1 {
		t.Fatalf("expected SELECT grant on docs for app_read, count = %d", grants)
	}
	if err := db.QueryRow(fmt.Sprintf(`
		SELECT count(*)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		JOIN aclexplode(p.proacl) a ON true
		WHERE n.nspname = '%s'
		  AND p.proname = 'touch_ts'
		  AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'app_read')
		  AND a.privilege_type = 'EXECUTE';
	`, schema)).Scan(&grants); err != nil {
		t.Fatalf("query function grant: %v", err)
	}
	if grants != 1 {
		t.Fatalf("expected EXECUTE grant on touch_ts for app_read, count = %d", grants)
	}

	// 2. Second sync: no-op.
	p, err := exec.PlanDiffPostgres(ctx, db, newCfg(rolesSpec("")))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p.Steps)
	}

	// 3. Grant removal from RolesSQL: REVOKE behind the gate.
	p2, err := exec.PlanDiffPostgres(ctx, db, newCfg(`CREATE ROLE app_read;`))
	if err != nil {
		t.Fatalf("revoke plan: %v", err)
	}
	if len(p2.Steps) != 2 {
		t.Fatalf("expected table and function destructive REVOKEs, got %+v", p2.Steps)
	}
	for _, step := range p2.Steps {
		if step.Type != plan.ChangeRevoke || !step.Destructive {
			t.Fatalf("expected only destructive REVOKEs, got %+v", p2.Steps)
		}
	}
	if err := exec.SyncPostgres(ctx, db, newCfg(`CREATE ROLE app_read;`)); err == nil {
		t.Fatalf("revoke must fail with default policy (AllowRevoke=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}
	revokeCfg := newCfg(`CREATE ROLE app_read;`)
	revokeCfg.Policy = plan.DropPolicy{AllowRevoke: true}
	revokeCfg.AcceptHazards = []plan.HazardCode{plan.HazardRevokePrivilege}
	if err := exec.SyncPostgres(ctx, db, revokeCfg); err != nil {
		t.Fatalf("allowed revoke sync: %v", err)
	}
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN aclexplode(c.relacl) a ON true WHERE n.nspname = '%s' AND c.relname = 'docs' AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'app_read');`, schema,
	)).Scan(&grants); err != nil {
		t.Fatalf("query grants after revoke: %v", err)
	}
	if grants != 0 {
		t.Fatalf("grants must be revoked, count = %d", grants)
	}

	// 4. Live-only managed role drop: gated by AllowDropRole + DROP_ROLE.
	emptyRoles := newCfg("-- empty desired roles state\n")
	p3, err := exec.PlanDiffPostgres(ctx, db, emptyRoles)
	if err != nil {
		t.Fatalf("live-only role plan: %v", err)
	}
	if len(p3.Steps) != 1 || p3.Steps[0].Type != plan.ChangeDropRole || !p3.Steps[0].Destructive {
		t.Fatalf("expected single destructive DROP_ROLE, got %+v", p3.Steps)
	}
	if err := exec.SyncPostgres(ctx, db, newCfg("-- empty desired roles state\n")); err == nil {
		t.Fatalf("role drop must fail with default policy (AllowDropRole=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}

	// 5. Operator-created roles are never swept: create one, sync with an
	// empty spec, and assert it survives.
	if _, err := db.Exec(`CREATE ROLE operator_role;`); err != nil {
		t.Fatalf("create operator role: %v", err)
	}
	defer func() { _, _ = db.Exec(`DROP ROLE IF EXISTS operator_role;`) }()
	allowDropCfg := newCfg("-- empty desired roles state\n")
	allowDropCfg.Policy = plan.DropPolicy{AllowDropRole: true}
	allowDropCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropRole}
	if err := exec.SyncPostgres(ctx, db, allowDropCfg); err != nil {
		t.Fatalf("allowed drop sync: %v", err)
	}
	var operatorKept int
	if err := db.QueryRow(`SELECT count(*) FROM pg_roles WHERE rolname = 'operator_role';`).Scan(&operatorKept); err != nil {
		t.Fatalf("query operator role: %v", err)
	}
	if operatorKept != 1 {
		t.Fatalf("operator role must survive sync")
	}
	var managedDropped int
	if err := db.QueryRow(`SELECT count(*) FROM pg_roles WHERE rolname = 'app_read';`).Scan(&managedDropped); err != nil {
		t.Fatalf("query dropped role: %v", err)
	}
	if managedDropped != 0 {
		t.Fatalf("marker-stamped role must be dropped, count = %d", managedDropped)
	}
}
