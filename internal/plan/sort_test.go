package plan_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/plan"
)

func TestSortSteps_CrossSchemaFK(t *testing.T) {
	// Scenario: billing.accounts references identity.users
	// billing.invoices references billing.accounts
	steps := []plan.Step{
		{
			Type:      plan.ChangeCreateTable,
			Schema:    "billing",
			Table:     "invoices",
			DependsOn: []string{"billing.accounts"},
		},
		{
			Type:      plan.ChangeCreateTable,
			Schema:    "billing",
			Table:     "accounts",
			DependsOn: []string{"identity.users"},
		},
		{
			Type:   plan.ChangeCreateTable,
			Schema: "identity",
			Table:  "users",
		},
	}

	plan.SortSteps(steps)

	expectedOrder := []struct {
		schema string
		table  string
	}{
		{"identity", "users"},
		{"billing", "accounts"},
		{"billing", "invoices"},
	}

	for i, exp := range expectedOrder {
		if steps[i].Schema != exp.schema || steps[i].Table != exp.table {
			t.Errorf("step %d: expected %s.%s, got %s.%s", i, exp.schema, exp.table, steps[i].Schema, steps[i].Table)
		}
	}
}

func TestSortSteps_CrossSchemaFKDrop(t *testing.T) {
	// When dropping tables: referencing tables must be dropped before referenced tables
	// billing.invoices -> billing.accounts -> identity.users
	steps := []plan.Step{
		{
			Type:   plan.ChangeDropTable,
			Schema: "identity",
			Table:  "users",
		},
		{
			Type:      plan.ChangeDropTable,
			Schema:    "billing",
			Table:     "accounts",
			DependsOn: []string{"identity.users"},
		},
		{
			Type:      plan.ChangeDropTable,
			Schema:    "billing",
			Table:     "invoices",
			DependsOn: []string{"billing.accounts"},
		},
	}

	plan.SortSteps(steps)

	expectedOrder := []struct {
		schema string
		table  string
	}{
		{"billing", "invoices"},
		{"billing", "accounts"},
		{"identity", "users"},
	}

	for i, exp := range expectedOrder {
		if steps[i].Schema != exp.schema || steps[i].Table != exp.table {
			t.Errorf("step %d: expected %s.%s, got %s.%s", i, exp.schema, exp.table, steps[i].Schema, steps[i].Table)
		}
	}
}

func TestSortSteps_AddFKInference(t *testing.T) {
	// If ChangeCreateTable has no explicit DependsOn, but ChangeAddFK specifies RefTable:
	steps := []plan.Step{
		{
			Type:   plan.ChangeCreateTable,
			Schema: "billing",
			Table:  "accounts",
		},
		{
			Type:   plan.ChangeCreateTable,
			Schema: "identity",
			Table:  "users",
		},
		{
			Type:     plan.ChangeAddFK,
			Schema:   "billing",
			Table:    "accounts",
			RefTable: "identity.users",
		},
	}

	plan.SortSteps(steps)

	// Users table must be created before accounts table
	// And ChangeAddFK must come after both ChangeCreateTable steps
	if steps[0].Schema != "identity" || steps[0].Table != "users" {
		t.Errorf("step 0: expected identity.users, got %s.%s", steps[0].Schema, steps[0].Table)
	}
	if steps[1].Schema != "billing" || steps[1].Table != "accounts" {
		t.Errorf("step 1: expected billing.accounts, got %s.%s", steps[1].Schema, steps[1].Table)
	}
	if steps[2].Type != plan.ChangeAddFK {
		t.Errorf("step 2: expected ChangeAddFK, got %v", steps[2].Type)
	}
}

// TestSortSteps_CycleIsDeterministic verifies circular CREATE_TABLE
// dependencies (legal when FKs are deferred) produce a stable lexical
// remainder rather than violating the sort contract.
func TestSortSteps_CycleIsDeterministic(t *testing.T) {
	mk := func() []plan.Step {
		return []plan.Step{
			{Type: plan.ChangeCreateTable, Schema: "public", Table: "b", DependsOn: []string{"public.a"}},
			{Type: plan.ChangeCreateTable, Schema: "public", Table: "a", DependsOn: []string{"public.b"}},
			{Type: plan.ChangeCreateTable, Schema: "public", Table: "c"},
		}
	}
	var first []string
	for i := range 20 {
		steps := mk()
		plan.SortSteps(steps)
		order := make([]string, len(steps))
		for j, s := range steps {
			order[j] = s.Table
		}
		if i == 0 {
			first = order
			continue
		}
		for j := range order {
			if order[j] != first[j] {
				t.Fatalf("iteration %d: non-deterministic order: got %v want %v", i, order, first)
			}
		}
	}
	// Independent table c has no deps → emitted first; a/b cycle remainder is lexical.
	if first[0] != "c" {
		t.Errorf("expected independent table c first, got %v", first)
	}
	if first[1] != "a" || first[2] != "b" {
		t.Errorf("expected cycle remainder a then b (lexical), got %v", first)
	}
}
