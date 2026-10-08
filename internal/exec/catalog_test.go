package exec

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

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

func TestValidateCatalogServerVersionRejectsPublicationSchemasBeforePostgres15(t *testing.T) {
	spec := schema.ParseCatalogSQL(`CREATE PUBLICATION docs_pub FOR TABLES IN SCHEMA docs;`)
	err := validateCatalogServerVersion(spec, 140000)
	if err == nil {
		t.Fatal("expected FOR TABLES IN SCHEMA to be rejected on PostgreSQL 14")
	}
	if !strings.Contains(err.Error(), "requires PostgreSQL 15 or newer") {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if !strings.Contains(err.Error(), plan.ErrInvalidOptions.Error()) {
		t.Fatalf("expected ErrInvalidOptions, got %v", err)
	}
	if err := validateCatalogServerVersion(spec, 150000); err != nil {
		t.Fatalf("PostgreSQL 15 must accept schema publication membership: %v", err)
	}
}
