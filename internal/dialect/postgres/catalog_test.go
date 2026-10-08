package postgres

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func TestGenerateCreateEventTriggerSQL_Disabled(t *testing.T) {
	sql := GenerateCreateEventTriggerSQL(&schema.EventTrigger{
		Name:     "audit",
		Event:    "ddl_command_end",
		Function: "log_ddl",
		Enabled:  false,
	})
	if !strings.Contains(sql, "CREATE EVENT TRIGGER") ||
		!strings.Contains(sql, `ALTER EVENT TRIGGER "audit" DISABLE;`) {
		t.Fatalf("disabled CREATE must include DISABLE: %s", sql)
	}
}

func TestGenerateCatalogSQL_EscapesQuotedIdentifiersAndTags(t *testing.T) {
	publicationSQL := GenerateCreatePublicationSQL(&schema.Publication{
		Name:   `pub"name`,
		Tables: []string{`"weird.schema"."table""name"`},
	})
	if !strings.Contains(publicationSQL, `CREATE PUBLICATION "pub""name" FOR TABLE "weird.schema"."table""name"`) {
		t.Fatalf("publication identifiers must use PostgreSQL quoting: %s", publicationSQL)
	}
	triggerSQL := GenerateCreateEventTriggerSQL(&schema.EventTrigger{
		Name:     `trig"name`,
		Event:    "ddl_command_end",
		Tags:     []string{"O'TABLE"},
		Function: `fn"name`,
	})
	if !strings.Contains(triggerSQL, `CREATE EVENT TRIGGER "trig""name"`) ||
		!strings.Contains(triggerSQL, `WHEN TAG IN ('O''TABLE')`) ||
		!strings.Contains(triggerSQL, `EXECUTE FUNCTION "fn""name"()`) {
		t.Fatalf("event-trigger identifiers/tags must be escaped: %s", triggerSQL)
	}
}

func TestGenerateCreatePublicationSQL_MixedMembershipConverges(t *testing.T) {
	sql := GenerateCreatePublicationSQL(&schema.Publication{
		Name:    "docs_pub",
		Tables:  []string{"public.docs"},
		Schemas: []string{"analytics"},
	})
	if !strings.Contains(sql, `CREATE PUBLICATION "docs_pub" FOR TABLE "public"."docs"`) ||
		!strings.Contains(sql, `ALTER PUBLICATION "docs_pub" ADD TABLES IN SCHEMA "analytics";`) {
		t.Fatalf("mixed publication membership must use valid CREATE plus ALTER SQL: %s", sql)
	}
}

func TestGenerateCatalogSQL_DoublesMaliciousIdentifierQuotes(t *testing.T) {
	publicationSQL := GenerateCreatePublicationSQL(&schema.Publication{
		Name:   `pub"; DROP PUBLICATION other; --`,
		Tables: []string{`"schema""name"."table""name"`},
	})
	if !strings.Contains(publicationSQL, `CREATE PUBLICATION "pub""; DROP PUBLICATION other; --"`) ||
		!strings.Contains(publicationSQL, `"schema""name"."table""name"`) {
		t.Fatalf("malicious publication identifiers must remain quoted: %s", publicationSQL)
	}
	triggerSQL := GenerateCreateEventTriggerSQL(&schema.EventTrigger{
		Name:     `trigger"; DROP EVENT TRIGGER other; --`,
		Event:    "ddl_command_end",
		Function: `"schema""name"."fn""name"`,
	})
	if !strings.Contains(triggerSQL, `CREATE EVENT TRIGGER "trigger""; DROP EVENT TRIGGER other; --"`) ||
		!strings.Contains(triggerSQL, `EXECUTE FUNCTION "schema""name"."fn""name"()`) {
		t.Fatalf("malicious event-trigger identifiers must remain quoted: %s", triggerSQL)
	}
}

func TestGenerateEventTriggerSQL_PreservesQualifiedQuotedFunction(t *testing.T) {
	sql := GenerateCreateEventTriggerSQL(&schema.EventTrigger{
		Name:     "audit",
		Event:    "ddl_command_end",
		Function: `"audit.schema"."Fn"`,
		Enabled:  true,
	})
	if !strings.Contains(sql, `EXECUTE FUNCTION "audit.schema"."Fn"()`) {
		t.Fatalf("qualified quoted function identity was not preserved: %s", sql)
	}
}

func TestGenerateAlterEventTriggerSQL_DefinitionReplacementPreservesDisabled(t *testing.T) {
	sql := GenerateAlterEventTriggerSQL(
		&schema.EventTrigger{
			Name:     "audit",
			Event:    "ddl_command_end",
			Function: "log_ddl_v2",
			Enabled:  false,
		},
		&schema.EventTrigger{
			Name:     "audit",
			Event:    "ddl_command_end",
			Function: "log_ddl",
			Enabled:  true,
		},
	)
	if !strings.Contains(sql, `CREATE EVENT TRIGGER "audit"`) ||
		!strings.Contains(sql, `ALTER EVENT TRIGGER "audit" DISABLE;`) {
		t.Fatalf("definition replacement must recreate disabled trigger: %s", sql)
	}
}

func TestRenderAlterEventTrigger_OnlyChangesEnabledState(t *testing.T) {
	step := RenderChange("public", diff.Change{
		Type: plan.ChangeAlterEventTrigger,
		EventTrigger: &schema.EventTrigger{
			Name:    "audit",
			Enabled: true,
		},
		OldEventTrigger: &schema.EventTrigger{
			Name:    "audit",
			Enabled: false,
		},
	})
	if step.SQL != `ALTER EVENT TRIGGER "audit" ENABLE;` || step.Destructive {
		t.Fatalf("enabled-state drift must render a non-destructive ALTER: %+v", step)
	}
}

func TestGenerateAlterPublicationSQL_AllTablesTransitions(t *testing.T) {
	old := &schema.Publication{
		Name:            "docs_pub",
		AllTables:       true,
		PublishInsert:   true,
		PublishUpdate:   true,
		PublishDelete:   true,
		PublishTruncate: true,
	}
	explicit := &schema.Publication{
		Name:            "docs_pub",
		Tables:          []string{"public.docs"},
		PublishInsert:   true,
		PublishUpdate:   true,
		PublishDelete:   true,
		PublishTruncate: true,
	}
	sql := GenerateAlterPublicationSQL(explicit, old)
	if !strings.Contains(sql, `ALTER PUBLICATION "docs_pub" SET TABLE "public"."docs";`) ||
		strings.Contains(sql, `ADD TABLE "public"."docs"`) {
		t.Fatalf("ALL TABLES -> explicit membership must emit one SET without duplicate ADD: %s", sql)
	}

	both := *explicit
	both.Schemas = []string{"analytics"}
	sql = GenerateAlterPublicationSQL(&both, old)
	if !strings.Contains(sql, `ALTER PUBLICATION "docs_pub" SET TABLE "public"."docs";`) ||
		!strings.Contains(sql, `ALTER PUBLICATION "docs_pub" ADD TABLES IN SCHEMA "analytics";`) ||
		strings.Contains(sql, `ADD TABLE "public"."docs"`) {
		t.Fatalf("ALL TABLES -> tables plus schemas must emit one SET and one schema ADD: %s", sql)
	}

	empty := &schema.Publication{
		Name:            "docs_pub",
		PublishInsert:   true,
		PublishUpdate:   true,
		PublishDelete:   true,
		PublishTruncate: true,
	}
	sql = GenerateAlterPublicationSQL(empty, old)
	if sql != "" || strings.Contains(sql, "DROP PUBLICATION") || strings.Contains(sql, "CREATE PUBLICATION") {
		t.Fatalf("ALL TABLES -> empty explicit membership must be represented by gated diff replacement: %s", sql)
	}

	oldExplicit := &schema.Publication{
		Name:            "docs_pub",
		Tables:          []string{"public.docs"},
		PublishInsert:   true,
		PublishUpdate:   true,
		PublishDelete:   true,
		PublishTruncate: true,
	}
	sql = GenerateAlterPublicationSQL(empty, oldExplicit)
	if !strings.Contains(sql, `ALTER PUBLICATION "docs_pub" DROP TABLE "public"."docs";`) {
		t.Fatalf("explicit membership -> empty must drop existing table membership: %s", sql)
	}
}

func TestEventTriggerEnabledStatus(t *testing.T) {
	for _, test := range []struct {
		status  string
		enabled bool
	}{
		{status: "O", enabled: true},
		{status: "A", enabled: true},
		{status: "R", enabled: true},
		{status: "D", enabled: false},
	} {
		t.Run(test.status, func(t *testing.T) {
			if got := eventTriggerEnabled(test.status); got != test.enabled {
				t.Fatalf("eventTriggerEnabled(%q) = %v, want %v", test.status, got, test.enabled)
			}
		})
	}
}

func TestMapQualifiedFunctionSchema(t *testing.T) {
	shadowMap := map[string]string{"billing": `_grizzle_shadow_billing`}
	if got, want := mapQualifiedFunctionSchema(`"billing"."AuditFn"`, shadowMap), `"_grizzle_shadow_billing"."AuditFn"`; got != want {
		t.Fatalf("mapped qualified function = %q, want %q", got, want)
	}
	if got := mapQualifiedFunctionSchema(`"other"."AuditFn"`, shadowMap); got != `"other"."AuditFn"` {
		t.Fatalf("unmapped qualified function changed: %q", got)
	}
}
