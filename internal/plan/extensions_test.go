package plan_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/plan"
)

func TestPlan_DropPolicy_NewObjectKinds(t *testing.T) {
	destructive := []plan.Step{
		{Type: plan.ChangeDropExtension, SQL: `DROP EXTENSION "x";`, Destructive: true},
		{Type: plan.ChangeDropFunction, SQL: `DROP FUNCTION "f";`, Destructive: true},
		{Type: plan.ChangeDropPolicy, SQL: `DROP POLICY "p" ON "t";`, Destructive: true},
		{Type: plan.ChangeDropTrigger, SQL: `DROP TRIGGER "tr" ON "t";`, Destructive: true},
		{Type: plan.ChangeDropView, SQL: `DROP VIEW "v";`, Destructive: true},
	}

	policy := plan.DropPolicy{}
	for _, s := range destructive {
		if policy.IsAllowed(s) {
			t.Errorf("%s must be denied by default policy", s.Type)
		}
	}

	policy = plan.DropPolicy{
		AllowExtension: true, AllowFunction: true, AllowPolicy: true,
		AllowTrigger: true, AllowView: true,
	}
	for _, s := range destructive {
		if !policy.IsAllowed(s) {
			t.Errorf("%s must be allowed when its AllowDrop flag is set", s.Type)
		}
	}
}

func TestPlan_SortExtensionsBeforeEnumsAndTables(t *testing.T) {
	steps := []plan.Step{
		{Type: plan.ChangeCreateTable, Table: "users", SQL: `CREATE TABLE "public"."users" (id int);`},
		{Type: plan.ChangeCreateEnum, Table: "status", SQL: `CREATE TYPE "public"."status" AS ENUM ('a');`},
		{Type: plan.ChangeCreateExtension, Table: "citext", SQL: `CREATE EXTENSION IF NOT EXISTS "citext";`},
	}
	plan.SortSteps(steps)
	if steps[0].Type != plan.ChangeCreateExtension {
		t.Fatalf("extension must sort first, got %s", steps[0].Type)
	}
	if steps[1].Type != plan.ChangeCreateEnum {
		t.Fatalf("enum must sort before tables, got %s", steps[1].Type)
	}
}

func TestPlan_Hazards_ExtensionAndRLS(t *testing.T) {
	p := &plan.Plan{Steps: []plan.Step{
		{Type: plan.ChangeCreateExtension, Table: "pgcrypto", SQL: `CREATE EXTENSION IF NOT EXISTS "pgcrypto";`},
		{Type: plan.ChangeEnableRLS, Table: "users", SQL: `ALTER TABLE "public"."users" ENABLE ROW LEVEL SECURITY;`},
		{Type: plan.ChangeDropPolicy, Table: "users", SQL: `DROP POLICY "p" ON "users";`, Destructive: true},
	}}
	hazards := p.Hazards()
	codes := map[plan.HazardCode]bool{}
	for _, h := range hazards {
		codes[h.Code] = true
	}
	for _, want := range []plan.HazardCode{
		plan.HazardExtensionPrivilege, plan.HazardRLSEnable, plan.HazardDropPolicy,
	} {
		if !codes[want] {
			t.Errorf("expected hazard %s, got %v", want, codes)
		}
	}
}
