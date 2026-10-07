package plan_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/plan"
)

// TestPlan_Hash_StableAcrossMapOrder verifies Hash() is invariant to the
// insertion order of map-backed fields (Renames) and unsorted slices
// (IncludeTables/ExcludeTables). A hash that varies across Go runs would
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
		Steps:         []plan.Step{{Type: plan.ChangeAddColumn, Table: "users", SQL: `ALTER TABLE "public"."users" ADD COLUMN name TEXT;`}},
		IncludeTables: []string{"users", "posts"},
		Renames:       map[string]string{"users_accounts": "users"},
	}

	const want = "821db232a94f4a000c4ab8a0a671a361a9709d4e5ada7bf5af76760b28ec5ab5"
	if got := p.Hash(); got != want {
		t.Errorf("golden hash mismatch:\n got  %s\n want %s\nIf this change is intentional (hash format edit), update the golden value.", got, want)
	}
}
