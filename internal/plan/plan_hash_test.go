package plan_test

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/plan"
)

// TestPlan_Hash_StableAcrossMapOrder verifies Hash() is invariant to the
// insertion order of map-backed fields (Renames) and unsorted slices
// (IncludeTables/ExcludeTables/TargetSchemas). A hash that varies across Go runs would
// falsely trip the ExpectedHash drift check for byte-identical plans.
func TestPlan_Hash_StableAcrossMapOrder(t *testing.T) {
	newPlan := func(renames map[string]string, includes, excludes []string) *plan.Plan {
		return &plan.Plan{
			TargetSchema:  "public",
			Steps:         []plan.Step{{Type: plan.ChangeAddColumn, Table: "users", SQL: `ALTER TABLE "public"."users" ADD COLUMN name TEXT;`}},
			IncludeTables: includes,
			ExcludeTables: excludes,
			Renames:       renames,
		}
	}

	renamesForward := map[string]string{"users_accounts": "users", "user_profiles": "profiles"}
	renamesReverse := map[string]string{"user_profiles": "profiles", "users_accounts": "users"}

	// Deterministic anchor: same content, different map insertion order.
	for i := range 50 {
		p1 := newPlan(renamesForward, []string{"users", "posts"}, []string{"internal_log"})
		p2 := newPlan(renamesReverse, []string{"posts", "users"}, []string{"internal_log"})

		h1, h2 := p1.Hash(), p2.Hash()
		if h1 != h2 {
			t.Fatalf("iteration %d: hash depends on map/slice order: %s != %s", i, h1, h2)
		}

		// Content change must change the hash (drift detection stays meaningful).
		p3 := newPlan(renamesReverse, []string{"posts", "users"}, []string{"internal_log", "audit"})
		if p3.Hash() == h1 {
			t.Fatalf("iteration %d: hash unchanged after exclude list change", i)
		}
	}
}

// TestPlan_Hash_Golden pins the hash of a fixed plan so any format change is
// a deliberate, reviewed event (records in grizzle_history key drift checks
// on this digest).
func TestPlan_Hash_Golden(t *testing.T) {
	p := &plan.Plan{
		TargetSchema:  "public",
		TargetSchemas: []string{"public"},
		Steps:         []plan.Step{{Type: plan.ChangeAddColumn, Table: "users", SQL: `ALTER TABLE "public"."users" ADD COLUMN name TEXT;`}},
		IncludeTables: []string{"users", "posts"},
		Renames:       map[string]string{"users_accounts": "users"},
	}

	const want = "fed488b287bc127c043ff6e746a7109f0c5f7589fd335b0a3e51064b2d20ed7e"
	if got := p.Hash(); got != want {
		t.Errorf("golden hash mismatch:\n got  %s\n want %s\nIf this change is intentional (hash format edit), update the golden value.", got, want)
	}
}

// TestPlan_Hash_ApprovalSensitiveFields verifies that mutating any
// approval-sensitive field changes the plan hash (integrity boundary).
func TestPlan_Hash_ApprovalSensitiveFields(t *testing.T) {
	base := &plan.Plan{
		TargetSchema:  "public",
		TargetSchemas: []string{"public"},
		Policy:        plan.DropPolicy{AllowIndex: true},
		IncludeTables: []string{"users"},
		ExcludeTables: []string{"audit"},
		Renames:       map[string]string{"a": "b"},
		SchemaSQL:     "CREATE TABLE users (id INT);",
		RolesSQL:      "CREATE ROLE app_read;",
		CatalogSQL:    "CREATE PUBLICATION docs FOR TABLE users;",
		Steps: []plan.Step{
			{Type: plan.ChangeAddColumn, Table: "users", SQL: `ALTER TABLE "public"."users" ADD COLUMN name TEXT;`},
		},
	}
	baseHash := base.Hash()

	cases := []struct {
		name   string
		mutate func(p *plan.Plan)
	}{
		{"policy", func(p *plan.Plan) { p.Policy.AllowTable = true }},
		{"target_schema_without_target_schemas", func(p *plan.Plan) {
			p.TargetSchemas = nil
			p.TargetSchema = "billing"
		}},
		{"target_schemas", func(p *plan.Plan) { p.TargetSchemas = []string{"billing", "public"} }},
		{"include_tables", func(p *plan.Plan) { p.IncludeTables = []string{"users", "posts"} }},
		{"exclude_tables", func(p *plan.Plan) { p.ExcludeTables = []string{"audit", "log"} }},
		{"renames", func(p *plan.Plan) { p.Renames = map[string]string{"a": "c"} }},
		{"expand_contract", func(p *plan.Plan) { p.ExpandContract = true }},
		{"non_concurrent", func(p *plan.Plan) { p.NonConcurrentIndexes = true }},
		{"schema_sql", func(p *plan.Plan) { p.SchemaSQL = "CREATE TABLE users (id BIGINT);" }},
		{"roles_sql", func(p *plan.Plan) { p.RolesSQL = "CREATE ROLE app_write;" }},
		{"catalog_sql", func(p *plan.Plan) { p.CatalogSQL = "CREATE PUBLICATION docs FOR ALL TABLES;" }},
		{"step_sql", func(p *plan.Plan) { p.Steps[0].SQL = `ALTER TABLE "public"."users" ADD COLUMN age INT;` }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clone := *base
			clone.Policy = base.Policy
			clone.IncludeTables = append([]string(nil), base.IncludeTables...)
			clone.ExcludeTables = append([]string(nil), base.ExcludeTables...)
			clone.TargetSchemas = append([]string(nil), base.TargetSchemas...)
			clone.Renames = map[string]string{"a": "b"}
			clone.Steps = append([]plan.Step(nil), base.Steps...)
			tc.mutate(&clone)
			if clone.Hash() == baseHash {
				t.Fatalf("mutating %s did not change hash", tc.name)
			}
		})
	}
}

func TestPlan_Hash_TargetSchemasOneElementIsIncluded(t *testing.T) {
	base := &plan.Plan{TargetSchema: "public"}
	withOneTarget := &plan.Plan{TargetSchema: "public", TargetSchemas: []string{"billing"}}
	if base.Hash() == withOneTarget.Hash() {
		t.Fatal("one-element TargetSchemas must participate in the plan hash")
	}
}

// TestPlan_Document_OmitsLockAndShadow verifies the serialized artifact
// never carries lock identity or shadow schema (generated at apply time).
func TestPlan_Document_OmitsLockAndShadow(t *testing.T) {
	p := &plan.Plan{
		TargetSchema: "public",
		Steps:        []plan.Step{{Type: plan.ChangeAddColumn, Table: "users", SQL: "ALTER TABLE users ADD COLUMN x INT;"}},
		SchemaSQL:    "CREATE TABLE users (id INT, x INT);",
		RolesSQL:     "CREATE ROLE app_read;",
		CatalogSQL:   "CREATE PUBLICATION docs FOR TABLE users;",
	}
	data, err := p.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	s := string(data)
	for _, forbidden := range []string{`"lock_id"`, `"lock_namespace"`, `"shadow_schema"`, `"options_digest"`} {
		if strings.Contains(s, forbidden) {
			t.Errorf("document must not contain %s", forbidden)
		}
	}
	parsed, hash, err := plan.ParsePlanJSON(data)
	if err != nil {
		t.Fatalf("ParsePlanJSON: %v", err)
	}
	if hash != p.Hash() || parsed.Hash() != p.Hash() {
		t.Fatalf("round-trip hash mismatch: envelope=%s parsed=%s want=%s", hash, parsed.Hash(), p.Hash())
	}
	if parsed.RolesSQL != p.RolesSQL || parsed.CatalogSQL != p.CatalogSQL {
		t.Fatalf("side-channel SQL did not round-trip: roles=%q/%q catalog=%q/%q",
			parsed.RolesSQL, p.RolesSQL, parsed.CatalogSQL, p.CatalogSQL)
	}
}
