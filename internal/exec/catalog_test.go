package exec

import "testing"

func TestEventTriggerFunctionLookupUsesAllShadowSchemas(t *testing.T) {
	lookup, shadowMap := eventTriggerFunctionLookup(PostgresExecConfig{
		TargetSchemas: []string{"public", "Billing"},
		ShadowSchema:  "_grizzle_shadow",
	})
	if shadowMap["public"] == "" || shadowMap["Billing"] == "" {
		t.Fatalf("expected a shadow mapping for every target schema: %+v", shadowMap)
	}
	if len(lookup) != 5 ||
		lookup[0] != shadowMap["public"] ||
		lookup[1] != "public" ||
		lookup[2] != shadowMap["Billing"] ||
		lookup[3] != "Billing" ||
		lookup[4] != "public" {
		t.Fatalf("unexpected event-trigger lookup order: %v", lookup)
	}
}
