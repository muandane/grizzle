//go:build integration

package exec_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/history"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestRoles_FunctionGrantRejectsProcedure(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schemaName := fmt.Sprintf("test_role_fn_%d", time.Now().UnixNano())
	roleName := fmt.Sprintf("fn_grant_role_%d", time.Now().UnixNano())
	cfg := exec.PostgresExecConfig{
		TargetSchema: schemaName,
		SchemaSQL: fmt.Sprintf(`
			CREATE PROCEDURE proc_only()
			LANGUAGE plpgsql AS $$ BEGIN NULL; END; $$;
		`),
		RolesSQL: fmt.Sprintf(`CREATE ROLE %q; GRANT EXECUTE ON FUNCTION proc_only() TO %q;`, roleName, roleName),
		Filters:  scope.Filters{},
		Policy:   plan.DropPolicy{},
	}
	if _, err := exec.PlanDiffPostgres(ctx, db, cfg); err == nil {
		t.Fatal("FUNCTION grants to procedures must be rejected")
	} else if !strings.Contains(err.Error(), "aggregates, procedures, and window functions are not supported") {
		t.Fatalf("unexpected non-function grant error: %v", err)
	}
}

func TestRoles_FunctionGrantSkipsInvalidEarlierRoutine(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	shadowSchema := fmt.Sprintf("test_fn_shadow_%d", time.Now().UnixNano())
	targetSchema := fmt.Sprintf("test_fn_target_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE SCHEMA %q;
		CREATE SCHEMA %q;
		CREATE PROCEDURE %q.same_name() LANGUAGE plpgsql AS $$ BEGIN NULL; END; $$;
		CREATE FUNCTION %q.same_name() RETURNS integer LANGUAGE sql IMMUTABLE AS 'SELECT 1';
	`, shadowSchema, targetSchema, shadowSchema, targetSchema)); err != nil {
		t.Fatalf("create routine candidates: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf(`DROP SCHEMA %q CASCADE; DROP SCHEMA %q CASCADE;`, shadowSchema, targetSchema))
	}()
	spec := schema.ParseRolesSQL(`GRANT EXECUTE ON FUNCTION same_name() TO PUBLIC;`)
	if err := postgres.ValidateFunctionGrantTargets(
		ctx, db, spec, targetSchema, map[string]string{targetSchema: shadowSchema},
	); err != nil {
		t.Fatalf("valid later function must not be hidden by earlier procedure: %v", err)
	}
}

func TestApplyPostgresRefusesUnmanagedRoleDrop(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	roleName := fmt.Sprintf("unmanaged_drop_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf(`CREATE ROLE %q;`, roleName)); err != nil {
		t.Fatalf("create unmanaged role: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %q;`, roleName)) }()
	if err := history.EnsureTable(ctx, db, "postgres", "public"); err != nil {
		t.Fatalf("ensure history table: %v", err)
	}

	p := &plan.Plan{
		TargetSchema: "public",
		Policy:       plan.DropPolicy{AllowDropRole: true},
		Steps: []plan.Step{{
			Type:        plan.ChangeDropRole,
			Table:       roleName,
			SQL:         fmt.Sprintf(`DROP ROLE %q;`, roleName),
			Destructive: true,
		}},
	}
	err := exec.ApplyPostgres(ctx, db, p, exec.PostgresExecConfig{
		TargetSchema:  "public",
		Policy:        plan.DropPolicy{AllowDropRole: true},
		AcceptHazards: []plan.HazardCode{plan.HazardDropRole},
		LockTimeout:   5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "exact grizzle-managed marker is absent") {
		t.Fatalf("unmanaged role drop must be refused at apply time, got %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pg_roles WHERE rolname = $1;`, roleName).Scan(&count); err != nil {
		t.Fatalf("check unmanaged role: %v", err)
	}
	if count != 1 {
		t.Fatalf("unmanaged role must remain after refused apply, count=%d", count)
	}
}

func TestApplyPostgresRefusesDropWithUnrevokedRoleACL(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schemaName := fmt.Sprintf("test_drop_acl_%d", time.Now().UnixNano())
	roleName := fmt.Sprintf("managed_drop_acl_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE SCHEMA %q;
		CREATE TABLE %q.docs (id integer);
		CREATE ROLE %q;
		COMMENT ON ROLE %q IS 'grizzle-managed';
		GRANT SELECT ON TABLE %q.docs TO %q;
	`, schemaName, schemaName, roleName, roleName, schemaName, roleName)); err != nil {
		t.Fatalf("create managed role dependency: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE;`, schemaName))
		_, _ = db.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %q;`, roleName))
	}()
	if err := history.EnsureTable(ctx, db, "postgres", "public"); err != nil {
		t.Fatalf("ensure history table: %v", err)
	}

	p := &plan.Plan{
		TargetSchema: schemaName,
		Policy:       plan.DropPolicy{AllowDropRole: true},
		Steps: []plan.Step{{
			Type:        plan.ChangeDropRole,
			Table:       roleName,
			SQL:         fmt.Sprintf(`DROP ROLE %q;`, roleName),
			Destructive: true,
		}},
	}
	err := exec.ApplyPostgres(ctx, db, p, exec.PostgresExecConfig{
		TargetSchema:  schemaName,
		Policy:        plan.DropPolicy{AllowDropRole: true},
		AcceptHazards: []plan.HazardCode{plan.HazardDropRole},
		LockTimeout:   5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "plan does not revoke ACL privilege") {
		t.Fatalf("drop-only plan with ACL dependency must be refused, got %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pg_roles WHERE rolname = $1;`, roleName).Scan(&count); err != nil {
		t.Fatalf("check managed role: %v", err)
	}
	if count != 1 {
		t.Fatalf("managed role must remain after refused apply, count=%d", count)
	}
}

// TestRoles_Lifecycle verifies RolesSQL privilege sync end-to-end: role
// creation (NOLOGIN, marker-stamped), grants applied, second-sync no-op,
// revocation gated behind AllowRevoke + REVOKE_PRIVILEGE, live-only managed
// role drop behind AllowDropRole + DROP_ROLE, and ownership refusal.
func TestRoles_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_roles_%d", time.Now().UnixNano())
	roleName := fmt.Sprintf("app_read_%d", time.Now().UnixNano())
	operatorRole := fmt.Sprintf("operator_role_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
		_, _ = db.Exec(fmt.Sprintf("DROP ROLE IF EXISTS %q;", roleName))
		_, _ = db.Exec(fmt.Sprintf("DROP ROLE IF EXISTS %q;", operatorRole))
	}()

	rolesSpec := func(grantExtra string) string {
		return fmt.Sprintf(`
			CREATE ROLE %q;
			GRANT SELECT ON docs TO %q;
			GRANT EXECUTE ON FUNCTION touch_ts() TO %q;
			%s
		`, roleName, roleName, roleName, grantExtra)
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
		`SELECT rolsuper, rolcanlogin FROM pg_roles WHERE rolname = %s;`, quoteLiteral(roleName),
	)).Scan(&rolSuper, &rolCanLogin); err != nil {
		t.Fatalf("query role: %v", err)
	}
	if rolCanLogin {
		t.Fatalf("roles without LOGIN must stay NOLOGIN")
	}
	if rolSuper {
		t.Fatalf("managed roles must not be superuser")
	}
	var marker string
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT COALESCE((SELECT description FROM pg_shdescription d WHERE d.objoid = r.oid AND d.classoid = 'pg_authid'::regclass), '') FROM pg_roles r WHERE rolname = %s;`, quoteLiteral(roleName),
	)).Scan(&marker); err != nil {
		t.Fatalf("query role marker: %v", err)
	}
	if marker == "" {
		t.Fatalf("managed role must be marker-stamped")
	}
	var grants int
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN aclexplode(c.relacl) a ON true WHERE n.nspname = '%s' AND c.relname = 'docs' AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = %s) AND a.privilege_type = 'SELECT';`, schema, quoteLiteral(roleName),
	)).Scan(&grants); err != nil {
		t.Fatalf("query grant: %v", err)
	}
	if grants != 1 {
		t.Fatalf("expected SELECT grant on docs for %s, count = %d", roleName, grants)
	}
	if err := db.QueryRow(fmt.Sprintf(`
		SELECT count(*)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		JOIN aclexplode(p.proacl) a ON true
		WHERE n.nspname = '%s'
		  AND p.proname = 'touch_ts'
		  AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = %s)
		  AND a.privilege_type = 'EXECUTE';
	`, schema, quoteLiteral(roleName))).Scan(&grants); err != nil {
		t.Fatalf("query function grant: %v", err)
	}
	if grants != 1 {
		t.Fatalf("expected EXECUTE grant on touch_ts for %s, count = %d", roleName, grants)
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
	p2, err := exec.PlanDiffPostgres(ctx, db, newCfg(fmt.Sprintf(`CREATE ROLE %q;`, roleName)))
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
	if err := exec.SyncPostgres(ctx, db, newCfg(fmt.Sprintf(`CREATE ROLE %q;`, roleName))); err == nil {
		t.Fatalf("revoke must fail with default policy (AllowRevoke=false)")
	} else {
		var dve *plan.DestructiveViolationError
		if !errors.As(err, &dve) {
			t.Fatalf("expected destructive violation, got: %v", err)
		}
	}
	revokeCfg := newCfg(fmt.Sprintf(`CREATE ROLE %q;`, roleName))
	revokeCfg.Policy = plan.DropPolicy{AllowRevoke: true}
	revokeCfg.AcceptHazards = []plan.HazardCode{plan.HazardRevokePrivilege}
	if err := exec.SyncPostgres(ctx, db, revokeCfg); err != nil {
		t.Fatalf("allowed revoke sync: %v", err)
	}
	if err := db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN aclexplode(c.relacl) a ON true WHERE n.nspname = '%s' AND c.relname = 'docs' AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = %s);`, schema, quoteLiteral(roleName),
	)).Scan(&grants); err != nil {
		t.Fatalf("query grants after revoke: %v", err)
	}
	if grants != 0 {
		t.Fatalf("grants must be revoked, count = %d", grants)
	}

	// Reintroduce an ACL dependency so DROP ROLE must revoke it first.
	if _, err := db.Exec(fmt.Sprintf(`GRANT UPDATE ON TABLE "%s"."docs" TO %s;`, schema, quoteIdent(roleName))); err != nil {
		t.Fatalf("regrant role ACL before drop: %v", err)
	}

	// 4. Live-only managed role drop: gated by AllowDropRole + DROP_ROLE,
	// with AllowRevoke + REVOKE_PRIVILEGE for its remaining ACLs.
	emptyRoles := newCfg("-- empty desired roles state\n")
	p3, err := exec.PlanDiffPostgres(ctx, db, emptyRoles)
	if err != nil {
		t.Fatalf("live-only role plan: %v", err)
	}
	var sawDropRole, sawDropRevoke bool
	for _, step := range p3.Steps {
		switch step.Type {
		case plan.ChangeDropRole:
			sawDropRole = step.Destructive
		case plan.ChangeRevoke:
			sawDropRevoke = step.Destructive
		}
	}
	if !sawDropRole || !sawDropRevoke {
		t.Fatalf("expected destructive ACL revoke before DROP_ROLE, got %+v", p3.Steps)
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
	if _, err := db.Exec(fmt.Sprintf(`CREATE ROLE %q;`, operatorRole)); err != nil {
		t.Fatalf("create operator role: %v", err)
	}
	allowDropCfg := newCfg("-- empty desired roles state\n")
	allowDropCfg.Policy = plan.DropPolicy{AllowDropRole: true, AllowRevoke: true}
	allowDropCfg.AcceptHazards = []plan.HazardCode{plan.HazardDropRole, plan.HazardRevokePrivilege}
	if err := exec.SyncPostgres(ctx, db, allowDropCfg); err != nil {
		t.Fatalf("allowed drop sync: %v", err)
	}
	var operatorKept int
	if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM pg_roles WHERE rolname = %s;`, quoteLiteral(operatorRole))).Scan(&operatorKept); err != nil {
		t.Fatalf("query operator role: %v", err)
	}
	if operatorKept != 1 {
		t.Fatalf("operator role must survive sync")
	}
	var managedDropped int
	if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM pg_roles WHERE rolname = %s;`, quoteLiteral(roleName))).Scan(&managedDropped); err != nil {
		t.Fatalf("query dropped role: %v", err)
	}
	if managedDropped != 0 {
		t.Fatalf("marker-stamped role must be dropped, count = %d", managedDropped)
	}
}

// TestRoles_LoginPasswordConfigDrift verifies LOGIN + PASSWORD + SET apply,
// second-sync no-op, password drift hazard, and plan JSON/SQL redaction.
func TestRoles_LoginPasswordConfigDrift(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schemaName := fmt.Sprintf("test_role_cfg_%d", time.Now().UnixNano())
	roleName := fmt.Sprintf("login_role_%d", time.Now().UnixNano())
	secret := "s3cret-not-in-plan"
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaName)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaName))
		_, _ = db.Exec(fmt.Sprintf("DROP ROLE IF EXISTS %q;", roleName))
	}()

	rolesSQL := fmt.Sprintf(`
		CREATE ROLE %q LOGIN PASSWORD '%s' CONNECTION LIMIT 2;
		ALTER ROLE %q SET work_mem = '4096';
	`, roleName, secret, roleName)

	cfg := exec.PostgresExecConfig{
		TargetSchema:     schemaName,
		SchemaSQL:        `CREATE TABLE docs (id bigint PRIMARY KEY);`,
		RolesSQL:         rolesSQL,
		Filters:          scope.Filters{},
		Policy:           plan.DropPolicy{},
		LockTimeout:      5 * time.Second,
		StatementTimeout: 30 * time.Second,
		AcceptHazards:    []plan.HazardCode{plan.HazardPasswordChange},
	}

	p, err := exec.PlanDiffPostgres(ctx, db, cfg)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	docJSON, err := p.ToJSON()
	if err != nil {
		t.Fatalf("plan json: %v", err)
	}
	if strings.Contains(string(docJSON), secret) {
		t.Fatalf("plan JSON must not contain password plaintext")
	}
	var sawPasswordStep bool
	for _, step := range p.Steps {
		if strings.Contains(step.SQL, secret) {
			t.Fatalf("Step.SQL leaked password: %s", step.SQL)
		}
		if step.Type == plan.ChangeAlterRole && strings.Contains(strings.ToUpper(step.SQL), "PASSWORD") {
			sawPasswordStep = true
			if !strings.Contains(step.SQL, schema.PasswordRedacted) {
				t.Fatalf("password step must use redacted placeholder: %s", step.SQL)
			}
		}
	}
	if !sawPasswordStep {
		t.Fatalf("expected redacted PASSWORD alter step, got %+v", p.Steps)
	}
	hazards := p.Hazards()
	var sawPWHazard bool
	for _, h := range hazards {
		if h.Code == plan.HazardPasswordChange {
			sawPWHazard = true
			if strings.Contains(h.SQL, secret) {
				t.Fatalf("hazard SQL leaked password")
			}
		}
	}
	if !sawPWHazard {
		t.Fatalf("expected PASSWORD_CHANGE hazard, got %+v", hazards)
	}

	if err := exec.SyncPostgres(ctx, db, cfg); err != nil {
		t.Fatalf("sync: %v", err)
	}
	var canLogin bool
	var connLimit int
	if err := db.QueryRow(`SELECT rolcanlogin, rolconnlimit FROM pg_roles WHERE rolname = $1`, roleName).Scan(&canLogin, &connLimit); err != nil {
		t.Fatalf("query role: %v", err)
	}
	if !canLogin {
		t.Fatalf("LOGIN role must be able to log in")
	}
	if connLimit != 2 {
		t.Fatalf("connection limit = %d, want 2", connLimit)
	}
	var workMem string
	if err := db.QueryRow(`
		SELECT split_part(u.entry, '=', 2)
		FROM pg_db_role_setting s
		JOIN pg_roles r ON r.oid = s.setrole
		CROSS JOIN LATERAL unnest(s.setconfig) AS u(entry)
		WHERE r.rolname = $1 AND s.setdatabase = 0 AND u.entry LIKE 'work_mem=%'
	`, roleName).Scan(&workMem); err != nil {
		t.Fatalf("query role setting: %v", err)
	}
	if workMem != "4096" {
		t.Fatalf("work_mem = %q, want 4096", workMem)
	}

	p2, err := exec.PlanDiffPostgres(ctx, db, cfg)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p2.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p2.Steps)
	}

	// Password drift: rotate live password, expect PASSWORD_CHANGE again.
	if _, err := db.Exec(fmt.Sprintf(`ALTER ROLE %q PASSWORD 'other-secret';`, roleName)); err != nil {
		t.Fatalf("rotate live password: %v", err)
	}
	p3, err := exec.PlanDiffPostgres(ctx, db, cfg)
	if err != nil {
		t.Fatalf("drift plan: %v", err)
	}
	var sawPW bool
	for _, step := range p3.Steps {
		if step.Type == plan.ChangeAlterRole && strings.Contains(strings.ToUpper(step.SQL), "PASSWORD") {
			sawPW = true
			if strings.Contains(step.SQL, secret) || strings.Contains(step.SQL, "other-secret") {
				t.Fatalf("drift Step.SQL leaked password: %s", step.SQL)
			}
		}
	}
	if !sawPW {
		t.Fatalf("password drift must emit ALTER_ROLE PASSWORD, got %+v", p3.Steps)
	}
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
