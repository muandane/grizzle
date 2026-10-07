package diff_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

const trgDef = "CREATE TRIGGER trg_guard BEFORE INSERT ON public.docs FOR EACH ROW EXECUTE FUNCTION trg_noop()"

func triggerTable(name string, withTrigger bool, extraCols ...string) *schema.Table {
	t := newTable(name)
	for i, c := range extraCols {
		t.Columns[c] = &schema.Column{Name: c, DataType: "text", IsNullable: true, Position: i + 2}
	}
	if withTrigger {
		t.Triggers = map[string]*schema.Trigger{
			"trg_guard": {Name: "trg_guard", Definition: trgDef},
		}
	}
	return t
}

func TestDiff_Triggers_CreateMissing(t *testing.T) {
	live := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
		"docs": triggerTable("docs", false),
	}}
	desired := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
		"docs": triggerTable("docs", true),
	}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Type != plan.ChangeCreateTrigger || changes[0].Destructive {
		t.Fatalf("expected single non-destructive CREATE_TRIGGER, got %+v", changes)
	}
}

func TestDiff_Triggers_DefinitionDriftReplaces(t *testing.T) {
	drifted := "CREATE TRIGGER trg_guard BEFORE UPDATE ON public.docs FOR EACH ROW EXECUTE FUNCTION trg_noop()"
	live := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
		"docs": triggerTable("docs", true),
	}}
	desiredTbl := triggerTable("docs", true)
	desiredTbl.Triggers["trg_guard"].Definition = drifted
	desired := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{"docs": desiredTbl}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropTrigger] != 1 || types[plan.ChangeCreateTrigger] != 1 {
		t.Fatalf("definition drift must DROP+CREATE, got %v", types)
	}
	for _, c := range changes {
		if c.Type == plan.ChangeDropTrigger && !c.Destructive {
			t.Fatalf("DROP_TRIGGER must be destructive")
		}
	}
}

func TestDiff_Triggers_LiveOnlyDrops(t *testing.T) {
	live := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
		"docs": triggerTable("docs", true),
	}}
	desired := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
		"docs": triggerTable("docs", false),
	}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Type != plan.ChangeDropTrigger || !changes[0].Destructive {
		t.Fatalf("expected single destructive DROP_TRIGGER, got %+v", changes)
	}
}

func TestDiff_Triggers_NoOpWhenEqual(t *testing.T) {
	sch := func() *schema.Schema {
		return &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
			"docs": triggerTable("docs", true),
		}}
	}
	changes, err := diff.Diff(sch(), sch(), "public", "_shadow", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %+v", changes)
	}
}

func TestDiff_Triggers_SurvivingTriggerBlocksColumnDrop(t *testing.T) {
	live := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
		"docs": triggerTable("docs", true, "email"),
	}}
	// Desired: email dropped, trigger unchanged (survives).
	desired := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
		"docs": triggerTable("docs", true),
	}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	var drop *diff.Change
	for i := range changes {
		if changes[i].Type == plan.ChangeDropColumn {
			drop = &changes[i]
		}
	}
	if drop == nil {
		t.Fatalf("expected DROP_COLUMN, got %+v", changes)
	}
	found := false
	for _, d := range drop.UnmanagedDeps {
		if d == "TRIGGER:trg_guard" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected surviving trigger dep TRIGGER:trg_guard, got %v", drop.UnmanagedDeps)
	}
}

func TestDiff_Triggers_ReplacedTriggerDoesNotBlockColumnDrop(t *testing.T) {
	drifted := "CREATE TRIGGER trg_guard BEFORE UPDATE ON public.docs FOR EACH ROW EXECUTE FUNCTION trg_noop()"
	live := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{
		"docs": triggerTable("docs", true, "email"),
	}}
	desiredTbl := triggerTable("docs", false)
	desiredTbl.Triggers = map[string]*schema.Trigger{
		"trg_guard": {Name: "trg_guard", Definition: drifted},
	}
	desired := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{"docs": desiredTbl}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	for _, c := range changes {
		if c.Type == plan.ChangeDropColumn && len(c.UnmanagedDeps) != 0 {
			t.Fatalf("replaced trigger must not block column drop, got %v", c.UnmanagedDeps)
		}
	}
}
