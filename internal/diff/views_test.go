package diff_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func view(name, def string, cols ...string) *schema.View {
	return &schema.View{Name: name, Definition: def, Columns: cols}
}

func TestDiff_Views_CreateMissing(t *testing.T) {
	live := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{"docs": newTable("docs")}}
	desired := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": newTable("docs")},
		Views:  map[string]*schema.View{"docs_recent": view("docs_recent", "SELECT id FROM docs", "id")},
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Type != plan.ChangeCreateView || changes[0].Destructive {
		t.Fatalf("expected single non-destructive CREATE_VIEW, got %+v", changes)
	}
}

func TestDiff_Views_AppendColumnsReplaces(t *testing.T) {
	live := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": newTable("docs")},
		Views:  map[string]*schema.View{"v": view("v", "SELECT id FROM docs", "id")},
	}
	desired := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": newTable("docs")},
		Views:  map[string]*schema.View{"v": view("v", "SELECT id, id AS extra FROM docs", "id", "extra")},
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Type != plan.ChangeCreateView || !changes[0].Replace || changes[0].Destructive {
		t.Fatalf("append-only drift must be a non-destructive CREATE OR REPLACE, got %+v", changes)
	}
}

func TestDiff_Views_ColumnRemovalDropsAndCreates(t *testing.T) {
	live := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": newTable("docs")},
		Views:  map[string]*schema.View{"v": view("v", "SELECT id, payload FROM docs", "id", "payload")},
	}
	desired := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": newTable("docs")},
		Views:  map[string]*schema.View{"v": view("v", "SELECT id FROM docs", "id")},
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropView] != 1 || types[plan.ChangeCreateView] != 1 {
		t.Fatalf("column removal must DROP+CREATE, got %v", types)
	}
	for _, c := range changes {
		if c.Replace {
			t.Fatalf("column removal must not replace in place: %+v", c)
		}
	}
}

func TestDiff_Views_MatViewAlwaysDropsAndCreatesWithRefresh(t *testing.T) {
	live := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": newTable("docs")},
		Views: map[string]*schema.View{"summary": func() *schema.View {
			v := view("summary", "SELECT count(*) FROM docs", "count")
			v.IsMatView = true
			return v
		}()},
	}
	desiredTbl := newTable("docs")
	desired := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": desiredTbl},
		Views: map[string]*schema.View{"summary": func() *schema.View {
			v := view("summary", "SELECT count(*) AS total FROM docs", "total")
			v.IsMatView = true
			return v
		}()},
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropView] != 1 || types[plan.ChangeCreateView] != 1 || types[plan.ChangeRefreshMatView] != 1 {
		t.Fatalf("matview drift must DROP+CREATE+REFRESH, got %v", types)
	}
	if changes[0].Type != plan.ChangeDropView || !changes[0].Destructive {
		t.Fatalf("first step must be destructive DROP_VIEW, got %+v", changes)
	}
}

func TestDiff_Views_LiveOnlyIsDestructiveDrop(t *testing.T) {
	live := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": newTable("docs")},
		Views:  map[string]*schema.View{"legacy_v": view("legacy_v", "SELECT 1", "?column?")},
	}
	desired := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{"docs": newTable("docs")}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Type != plan.ChangeDropView || !changes[0].Destructive {
		t.Fatalf("expected single destructive DROP_VIEW, got %+v", changes)
	}
}

func TestDiff_Views_NoOpWhenEqual(t *testing.T) {
	sch := func() *schema.Schema {
		return &schema.Schema{Name: "public",
			Tables: map[string]*schema.Table{"docs": newTable("docs")},
			Views:  map[string]*schema.View{"v": view("v", "SELECT id FROM docs", "id")},
		}
	}
	changes, err := diff.Diff(sch(), sch(), "public", "_shadow", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %+v", changes)
	}
}

func TestDiff_Views_SurvivingViewDriftsInsteadOfBlocking(t *testing.T) {
	// The shadow compile guarantees views always compile against desired
	// tables, so a column a view references cannot be dropped while the view
	// stays unchanged: the view drifts to DROP_VIEW + CREATE_VIEW instead.
	liveTbl := newTable("docs")
	liveTbl.Columns["email"] = &schema.Column{Name: "email", DataType: "text", IsNullable: true, Position: 2}
	live := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": liveTbl},
		Views:  map[string]*schema.View{"v": view("v", "SELECT id, email FROM docs", "id", "email")},
	}
	// Desired: view rewritten without email; the dropped column compiles.
	desiredTbl := newTable("docs")
	desired := &schema.Schema{Name: "public",
		Tables: map[string]*schema.Table{"docs": desiredTbl},
		Views:  map[string]*schema.View{"v": view("v", "SELECT id FROM docs", "id")},
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropView] != 1 || types[plan.ChangeCreateView] != 1 || types[plan.ChangeDropColumn] != 1 {
		t.Fatalf("expected DROP_VIEW+CREATE_VIEW+DROP_COLUMN, got %v", types)
	}
	for _, c := range changes {
		if c.Type == plan.ChangeDropView && !c.Destructive {
			t.Fatalf("DROP_VIEW must be destructive: %+v", c)
		}
	}
}
