package lint

import (
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

func TestDisallowReservedKeyword(t *testing.T) {
	s := &schema.Schema{
		Tables: map[string]*schema.Table{
			"order": {
				Name: "order",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "bigint"},
					"where": {Name: "where", DataType: "text"},
				},
			},
			"customers": {
				Name: "customers",
				Columns: map[string]*schema.Column{
					"id":   {Name: "id", DataType: "bigint"},
					"name": {Name: "name", DataType: "text"},
				},
			},
		},
	}

	diags := Lint(s, DisallowReservedKeyword{})
	if len(diags) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d: %+v", len(diags), diags)
	}

	// Sorted by table, column, message
	if diags[0].Table != "order" || diags[0].Column != "" || diags[0].RuleID != "L022" {
		t.Errorf("unexpected table keyword diagnostic: %+v", diags[0])
	}
	if diags[1].Table != "order" || diags[1].Column != "where" || diags[1].RuleID != "L022" {
		t.Errorf("unexpected column keyword diagnostic: %+v", diags[1])
	}
}
