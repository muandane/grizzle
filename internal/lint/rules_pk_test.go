package lint

import (
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

func TestPrimaryKeyTypeAllowlist(t *testing.T) {
	s := &schema.Schema{
		Tables: map[string]*schema.Table{
			"valid_int": {
				Name: "valid_int",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "bigint"},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "valid_int_pk", Columns: []string{"id"}},
			},
			"valid_uuid": {
				Name: "valid_uuid",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "uuid"},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "valid_uuid_pk", Columns: []string{"id"}},
			},
			"invalid_text": {
				Name: "invalid_text",
				Columns: map[string]*schema.Column{
					"sku": {Name: "sku", DataType: "varchar(255)"},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "invalid_text_pk", Columns: []string{"sku"}},
			},
			"invalid_float": {
				Name: "invalid_float",
				Columns: map[string]*schema.Column{
					"score": {Name: "score", DataType: "double precision"},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "invalid_float_pk", Columns: []string{"score"}},
			},
		},
	}

	diags := Lint(s, PrimaryKeyTypeAllowlist{})
	if len(diags) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d: %+v", len(diags), diags)
	}

	if diags[0].Table != "invalid_float" || diags[0].Column != "score" || diags[0].RuleID != "L014" {
		t.Errorf("unexpected first diagnostic: %+v", diags[0])
	}
	if diags[1].Table != "invalid_text" || diags[1].Column != "sku" || diags[1].RuleID != "L014" {
		t.Errorf("unexpected second diagnostic: %+v", diags[1])
	}
}
