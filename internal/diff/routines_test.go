package diff_test

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func routine(name, identityArgs, returnType, definition string) *schema.Routine {
	return &schema.Routine{
		Name:         name,
		Kind:         "FUNCTION",
		IdentityArgs: identityArgs,
		ReturnType:   returnType,
		Language:     "plpgsql",
		Volatility:   "VOLATILE",
		Definition:   definition,
	}
}

func countTypes(changes []diff.Change) map[plan.ChangeType]int {
	types := map[plan.ChangeType]int{}
	for _, c := range changes {
		types[c.Type]++
	}
	return types
}

func TestDiff_Routines_CreateMissing(t *testing.T) {
	live := &schema.Schema{Name: "public"}
	desired := &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
		schema.RoutineKey("add_one", "integer"): routine("add_one", "integer", "integer", "CREATE OR REPLACE FUNCTION public.add_one(integer) RETURNS integer LANGUAGE plpgsql AS $$BEGIN RETURN 1;$$"),
	}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeCreateFunction] != 1 || types[plan.ChangeDropFunction] != 0 {
		t.Fatalf("expected single CREATE_FUNCTION, got %v", types)
	}
	for _, c := range changes {
		if c.Destructive {
			t.Fatalf("create must not be destructive: %+v", c)
		}
	}
}

func TestDiff_Routines_BodyDriftReplacesInPlace(t *testing.T) {
	liveDef := "CREATE OR REPLACE FUNCTION public.add_one(v integer) RETURNS integer LANGUAGE plpgsql AS $$BEGIN RETURN 1;$$"
	driftDef := "CREATE OR REPLACE FUNCTION public.add_one(v integer) RETURNS integer LANGUAGE plpgsql AS $$BEGIN RETURN v + 1;$$"
	sch := func(def string) *schema.Schema {
		return &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
			schema.RoutineKey("add_one", "v integer"): routine("add_one", "v integer", "integer", def),
		}}
	}

	changes, err := diff.Diff(sch(liveDef), sch(driftDef), "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeCreateFunction] != 1 || types[plan.ChangeDropFunction] != 0 {
		t.Fatalf("body drift must be a non-destructive replace, got %v", types)
	}
	for _, c := range changes {
		if c.Destructive {
			t.Fatalf("replace must not be destructive: %+v", c)
		}
	}
}

func TestDiff_Routines_SignatureChangeDropsAndCreates(t *testing.T) {
	live := &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
		schema.RoutineKey("add_one", "integer"): routine("add_one", "integer", "integer", "CREATE OR REPLACE FUNCTION public.add_one(integer) RETURNS integer LANGUAGE plpgsql AS $$BEGIN RETURN 1;$$"),
	}}
	desired := &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
		schema.RoutineKey("add_one", "integer, integer"): routine("add_one", "integer, integer", "integer", "CREATE OR REPLACE FUNCTION public.add_one(integer, integer) RETURNS integer LANGUAGE plpgsql AS $$BEGIN RETURN 1;$$"),
	}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropFunction] != 1 || types[plan.ChangeCreateFunction] != 1 {
		t.Fatalf("signature change must DROP+CREATE, got %v", types)
	}
	for _, c := range changes {
		if c.Type == plan.ChangeDropFunction && !c.Destructive {
			t.Fatalf("drop from signature change must be destructive: %+v", c)
		}
	}
}

func TestDiff_Routines_LiveOnlyIsDestructiveDrop(t *testing.T) {
	live := &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
		schema.RoutineKey("legacy_fn", ""): routine("legacy_fn", "", "void", "CREATE OR REPLACE FUNCTION public.legacy_fn() RETURNS void LANGUAGE sql AS $$SELECT 1$$"),
	}}
	desired := &schema.Schema{Name: "public"}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Type != plan.ChangeDropFunction || !changes[0].Destructive {
		t.Fatalf("expected single destructive DROP_FUNCTION, got %+v", changes)
	}
}

func TestDiff_Routines_ShadowNamesUnmappedInDefinition(t *testing.T) {
	live := &schema.Schema{Name: "public"}
	desired := &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
		schema.RoutineKey("uses_enum", "e"): routine("uses_enum", "e", "public.myenum",
			`CREATE OR REPLACE FUNCTION _grizzle_shadow.uses_enum(e public.myenum) RETURNS _grizzle_shadow.myenum LANGUAGE sql AS $$SELECT e$$`),
	}}

	changes, err := diff.Diff(live, desired, "public", "_grizzle_shadow", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected one change, got %+v", changes)
	}
	r := changes[0].Routine
	if r == nil || strings.Contains(r.Definition, "_grizzle_shadow") {
		t.Fatalf("definition must be unmapped from shadow schema, got %+v", r)
	}
	if strings.Contains(r.IdentityArgs, "_grizzle_shadow") || strings.Contains(r.ReturnType, "_grizzle_shadow") {
		t.Fatalf("signature must be unmapped from shadow schema, got %+v", r)
	}
}

func TestDiff_Routines_NoOpWhenNormalizedEqual(t *testing.T) {
	sch := func() *schema.Schema {
		return &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
			schema.RoutineKey("stable_fn", "x integer"): routine("stable_fn", "x integer", "integer",
				"CREATE OR REPLACE FUNCTION public.stable_fn(x integer) RETURNS integer LANGUAGE plpgsql AS $$BEGIN RETURN x;$$"),
		}}
	}

	changes, err := diff.Diff(sch(), sch(), "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %+v", changes)
	}
}

func aggregate(name, identityArgs, definition string) *schema.Routine {
	return &schema.Routine{
		Name:         name,
		Kind:         "AGGREGATE",
		IdentityArgs: identityArgs,
		Definition:   definition,
	}
}

func TestDiff_Routines_AggregateCreateMissing(t *testing.T) {
	live := &schema.Schema{Name: "public"}
	desired := &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
		schema.RoutineKey("sum2", "v integer"): aggregate("sum2", "v integer",
			"CREATE AGGREGATE public.sum2(v integer) (\n    SFUNC = int4pl,\n    STYPE = integer\n);"),
	}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeCreateAggregate] != 1 || types[plan.ChangeDropAggregate] != 0 {
		t.Fatalf("expected single CREATE_AGGREGATE, got %v", types)
	}
	for _, c := range changes {
		if c.Destructive {
			t.Fatalf("create must not be destructive: %+v", c)
		}
	}
}

func TestDiff_Routines_AggregateBodyDriftDropsAndCreates(t *testing.T) {
	liveDef := "CREATE AGGREGATE public.sum2(v integer) (\n    SFUNC = int4pl,\n    STYPE = integer\n);"
	driftDef := "CREATE AGGREGATE public.sum2(v integer) (\n    SFUNC = int8pl,\n    STYPE = bigint\n);"
	sch := func(def string) *schema.Schema {
		return &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
			schema.RoutineKey("sum2", "v integer"): aggregate("sum2", "v integer", def),
		}}
	}

	changes, err := diff.Diff(sch(liveDef), sch(driftDef), "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	// Aggregates have no CREATE OR REPLACE: body drift must be DROP + CREATE.
	if len(changes) != 2 {
		t.Fatalf("expected DROP+CREATE for aggregate drift, got %+v", changes)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropAggregate] != 1 || types[plan.ChangeCreateAggregate] != 1 {
		t.Fatalf("aggregate drift must DROP_AGGREGATE+CREATE_AGGREGATE, got %v", types)
	}
	for _, c := range changes {
		if c.Type == plan.ChangeDropAggregate && !c.Destructive {
			t.Fatalf("aggregate drop must be destructive: %+v", c)
		}
	}
	if changes[0].Type != plan.ChangeDropAggregate {
		t.Fatalf("drop must precede create, got %+v", changes)
	}
}

func TestDiff_Routines_AggregateNoOpWhenEqual(t *testing.T) {
	sch := func() *schema.Schema {
		return &schema.Schema{Name: "public", Routines: map[string]*schema.Routine{
			schema.RoutineKey("sum2", "v integer"): aggregate("sum2", "v integer",
				"CREATE AGGREGATE public.sum2(v integer) (\n    SFUNC = int4pl,\n    STYPE = integer\n);"),
		}}
	}

	changes, err := diff.Diff(sch(), sch(), "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %+v", changes)
	}
}
