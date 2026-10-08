package diff_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func roleSpec(t *testing.T, sql string) *schema.RolesSpec {
	t.Helper()
	return schema.ParseRolesSQL(sql)
}

func TestRolesDiff_CreateMissingRoleAndGrant(t *testing.T) {
	desired := roleSpec(t, `CREATE ROLE app_read; GRANT SELECT ON docs TO app_read;`)
	live := &diff.RoleState{
		RoleNames:    map[string]string{},
		ManagedRoles: map[string]bool{},
	}

	changes := diff.RolesDiff(desired, live, "public")
	types := countTypes(changes)
	if types[plan.ChangeCreateRole] != 1 || types[plan.ChangeRoleComment] != 1 || types[plan.ChangeGrant] != 1 {
		t.Fatalf("expected CREATE_ROLE + ROLE_COMMENT + GRANT, got %v", types)
	}
	for _, c := range changes {
		if c.Destructive {
			t.Fatalf("create/grant must not be destructive: %+v", c)
		}
	}
}

func TestRolesDiff_RevokeSurplusBehindGate(t *testing.T) {
	desired := roleSpec(t, `GRANT SELECT ON docs TO app_read;`)
	live := &diff.RoleState{
		RoleNames:    map[string]string{"app_read": "app_read"},
		ManagedRoles: map[string]bool{"app_read": true},
		Grants: []*diff.RoleACLGrant{{
			Grantee:    "APP_READ",
			ObjectKind: "TABLE",
			ObjectName: "public.docs",
			Privileges: []string{"INSERT", "SELECT"},
		}},
	}

	changes := diff.RolesDiff(desired, live, "public")
	types := countTypes(changes)
	if types[plan.ChangeRevoke] != 1 || types[plan.ChangeGrant] != 0 {
		t.Fatalf("expected single REVOKE for surplus INSERT, got %v", types)
	}
	if !changes[0].Destructive || changes[0].Grant == nil || len(changes[0].Grant.Privileges) != 1 || changes[0].Grant.Privileges[0] != "INSERT" {
		t.Fatalf("revoke must be destructive and limited to surplus: %+v", changes[0])
	}
}

func TestRolesDiff_UnmanagedGranteeLeftAlone(t *testing.T) {
	desired := roleSpec(t, `GRANT SELECT ON docs TO app_read;`)
	live := &diff.RoleState{
		RoleNames:    map[string]string{"app_read": "app_read", "legacy": "legacy"},
		ManagedRoles: map[string]bool{"app_read": true},
		Grants: []*diff.RoleACLGrant{
			{Grantee: "APP_READ", ObjectKind: "TABLE", ObjectName: "public.docs", Privileges: []string{"SELECT"}},
			{Grantee: "legacy", ObjectKind: "TABLE", ObjectName: "public.docs", Privileges: []string{"DELETE"}},
		},
	}

	changes := diff.RolesDiff(desired, live, "public")
	for _, c := range changes {
		if c.Type == plan.ChangeRevoke {
			t.Fatalf("operator grants to unmanaged grantees must not be revoked: %+v", c)
		}
	}
	if len(changes) != 0 {
		t.Fatalf("desired grant already applied; expected no changes, got %+v", changes)
	}
}

func TestRolesDiff_PublicLiveOnlyGrantUntouched(t *testing.T) {
	desired := roleSpec(t, `GRANT SELECT ON docs TO app_read;`)
	live := &diff.RoleState{
		RoleNames:    map[string]string{"app_read": "app_read"},
		ManagedRoles: map[string]bool{"app_read": true},
		Grants: []*diff.RoleACLGrant{
			{Grantee: "APP_READ", ObjectKind: "TABLE", ObjectName: "public.docs", Privileges: []string{"SELECT"}},
			{Grantee: "PUBLIC", ObjectKind: "TABLE", ObjectName: "public.docs", Privileges: []string{"USAGE"}},
		},
	}

	changes := diff.RolesDiff(desired, live, "public")
	if len(changes) != 0 {
		t.Fatalf("live-only PUBLIC grants are operator state; expected no changes, got %+v", changes)
	}
}

func TestRolesDiff_GrantOptionDrift(t *testing.T) {
	// Desired adds WITH GRANT OPTION: re-grant.
	desired := roleSpec(t, `GRANT SELECT ON docs TO app_read WITH GRANT OPTION;`)
	live := &diff.RoleState{
		RoleNames:    map[string]string{"app_read": "app_read"},
		ManagedRoles: map[string]bool{"app_read": true},
		Grants: []*diff.RoleACLGrant{
			{Grantee: "APP_READ", ObjectKind: "TABLE", ObjectName: "public.docs", Privileges: []string{"SELECT"}},
		},
	}
	changes := diff.RolesDiff(desired, live, "public")
	if len(changes) != 1 || changes[0].Type != plan.ChangeGrant || !changes[0].Grant.GrantOption {
		t.Fatalf("option drift must re-grant with option, got %+v", changes)
	}

	// Desired drops the option: revoke then plain re-grant.
	desired2 := roleSpec(t, `GRANT SELECT ON docs TO app_read;`)
	live2 := &diff.RoleState{
		RoleNames:    map[string]string{"app_read": "app_read"},
		ManagedRoles: map[string]bool{"app_read": true},
		Grants: []*diff.RoleACLGrant{
			{Grantee: "APP_READ", ObjectKind: "TABLE", ObjectName: "public.docs", Privileges: []string{"SELECT"}, GrantOptions: []string{"SELECT"}},
		},
	}
	changes2 := diff.RolesDiff(desired2, live2, "public")
	if len(changes2) != 1 || changes2[0].Type != plan.ChangeRevoke || !changes2[0].Grant.GrantOption {
		t.Fatalf("unwanted option must revoke the delegation only, got %+v", changes2)
	}
}

func TestRolesDiff_DropRoleRevokesItsGrantsBeforeDrop(t *testing.T) {
	desired := roleSpec(t, ``)
	live := &diff.RoleState{
		RoleNames:    map[string]string{"app_read": "app_read"},
		ManagedRoles: map[string]bool{"app_read": true},
		Grants: []*diff.RoleACLGrant{
			{Grantee: "APP_READ", ObjectKind: "TABLE", ObjectName: "public.docs", Privileges: []string{"SELECT"}},
		},
	}

	changes := diff.RolesDiff(desired, live, "public")
	types := countTypes(changes)
	if types[plan.ChangeDropRole] != 1 || types[plan.ChangeRevoke] != 1 {
		t.Fatalf("managed marker + absent desired must REVOKE ACLs before DROP_ROLE, got %v", types)
	}
	var sawRevoke, sawDrop bool
	for _, change := range changes {
		switch change.Type {
		case plan.ChangeRevoke:
			sawRevoke = true
			if !change.Destructive || change.Grant == nil || len(change.Grant.Privileges) != 1 {
				t.Fatalf("role ACL revoke must be destructive and precise: %+v", change)
			}
		case plan.ChangeDropRole:
			sawDrop = true
			if !change.Destructive {
				t.Fatalf("drop role must be destructive: %+v", change)
			}
		}
	}
	if !sawRevoke || !sawDrop {
		t.Fatalf("expected both revoke and drop changes: %+v", changes)
	}
}

func TestRolesDiff_OperatorRoleNeverSwept(t *testing.T) {
	desired := roleSpec(t, `GRANT SELECT ON docs TO app_read;`)
	live := &diff.RoleState{
		RoleNames:    map[string]string{"app_read": "app_read", "dba_alice": "dba_alice"},
		ManagedRoles: map[string]bool{"app_read": true}, // dba_alice has no marker
		Grants: []*diff.RoleACLGrant{
			{Grantee: "APP_READ", ObjectKind: "TABLE", ObjectName: "public.docs", Privileges: []string{"SELECT"}},
		},
	}

	changes := diff.RolesDiff(desired, live, "public")
	for _, c := range changes {
		if c.Type == plan.ChangeDropRole {
			t.Fatalf("roles without the managed marker must never be dropped: %+v", c)
		}
	}
}
