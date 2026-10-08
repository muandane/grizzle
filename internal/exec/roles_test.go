package exec

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func TestMaterializeRoleStepSQL_RebuildsPasswordFromIR(t *testing.T) {
	step := plan.Step{
		Type:  plan.ChangeAlterRole,
		Table: "app_login",
		SQL:   `ALTER ROLE "app_login" PASSWORD '********';`,
	}
	roles := map[string]*schema.Role{
		"app_login": {Name: "app_login", HasPassword: true, Password: "s3cret"},
	}
	sql, err := materializeRoleStepSQL(step, roles)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if strings.Contains(sql, schema.PasswordRedacted) {
		t.Fatalf("apply SQL must not keep redacted placeholder: %s", sql)
	}
	if !strings.Contains(sql, "s3cret") {
		t.Fatalf("apply SQL must use IR password: %s", sql)
	}
	if strings.Contains(step.SQL, "s3cret") {
		t.Fatal("Step.SQL must remain redacted")
	}
}

func TestMaterializeRoleStepSQL_RefusesRedactedOnlyPassword(t *testing.T) {
	step := plan.Step{
		Type:  plan.ChangeAlterRole,
		Table: "app_login",
		SQL:   `ALTER ROLE "app_login" PASSWORD '********';`,
	}
	cases := []struct {
		name  string
		roles map[string]*schema.Role
	}{
		{"nil IR", nil},
		{"missing role", map[string]*schema.Role{}},
		{"redacted placeholder IR", map[string]*schema.Role{
			"app_login": {Name: "app_login", HasPassword: true, Password: schema.PasswordRedacted},
		}},
		{"empty password", map[string]*schema.Role{
			"app_login": {Name: "app_login", HasPassword: true, Password: ""},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, err := materializeRoleStepSQL(step, tc.roles)
			if err == nil {
				t.Fatalf("expected refusal, got SQL %q", sql)
			}
			if !strings.Contains(err.Error(), "plaintext password IR is unavailable") {
				t.Fatalf("unexpected error: %v", err)
			}
			if sql != "" {
				t.Fatalf("refused apply must not return SQL, got %q", sql)
			}
		})
	}
}

func TestMaterializeRoleStepSQL_NonPasswordUnchanged(t *testing.T) {
	step := plan.Step{
		Type:  plan.ChangeAlterRole,
		Table: "app",
		SQL:   `ALTER ROLE "app" SET search_path = public;`,
	}
	sql, err := materializeRoleStepSQL(step, nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if sql != step.SQL {
		t.Fatalf("got %q, want %q", sql, step.SQL)
	}
}
