package lint_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/lint"
	"github.com/muandane/grizzle/internal/schema"
)

func TestRLSEnableWithoutPolicies_L008(t *testing.T) {
	s := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
		"with_rls_no_policies": {
			Name:       "with_rls_no_policies",
			Columns:    map[string]*schema.Column{"id": {Name: "id", DataType: "bigint", Position: 1}},
			PrimaryKey: &schema.PrimaryKey{Name: "pk", Columns: []string{"id"}},
			RLSEnabled: true,
		},
		"with_rls_and_policy": {
			Name:       "with_rls_and_policy",
			Columns:    map[string]*schema.Column{"id": {Name: "id", DataType: "bigint", Position: 1}},
			PrimaryKey: &schema.PrimaryKey{Name: "pk", Columns: []string{"id"}},
			RLSEnabled: true,
			Policies: map[string]*schema.Policy{
				"p": {Name: "p", Cmd: "ALL"},
			},
		},
		"no_rls": {
			Name:       "no_rls",
			Columns:    map[string]*schema.Column{"id": {Name: "id", DataType: "bigint", Position: 1}},
			PrimaryKey: &schema.PrimaryKey{Name: "pk", Columns: []string{"id"}},
		},
	}}

	diags := lint.Lint(s, lint.RLSEnableWithoutPolicies{})
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %+v", diags)
	}
	if diags[0].RuleID != "L008" || diags[0].Table != "with_rls_no_policies" {
		t.Fatalf("unexpected diagnostic: %+v", diags[0])
	}
	if diags[0].Severity != lint.SeverityWarning {
		t.Fatalf("L008 must be WARNING, got %s", diags[0].Severity)
	}
}
