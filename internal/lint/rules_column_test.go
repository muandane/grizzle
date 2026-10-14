package lint

import (
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

func TestRequireBooleanDefault(t *testing.T) {
	s := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"is_active":   {Name: "is_active", DataType: "boolean", DefaultValue: "true"},
					"is_verified": {Name: "is_verified", DataType: "bool", DefaultValue: "false"},
					"is_admin":    {Name: "is_admin", DataType: "boolean"}, // missing default!
				},
			},
		},
	}

	diags := Lint(s, RequireBooleanDefault{})
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(diags), diags)
	}

	if diags[0].Table != "users" || diags[0].Column != "is_admin" || diags[0].RuleID != "L044" {
		t.Errorf("unexpected diagnostic: %+v", diags[0])
	}
}
