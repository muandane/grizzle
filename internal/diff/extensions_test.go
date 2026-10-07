package diff_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func TestDiff_ExtensionCreate(t *testing.T) {
	live := &schema.Schema{Name: "public", Extensions: map[string]*schema.Extension{}}
	desired := &schema.Schema{
		Name: "public",
		Extensions: map[string]*schema.Extension{
			"pgcrypto": {Name: "pgcrypto"},
		},
	}
	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Type != plan.ChangeCreateExtension {
		t.Fatalf("expected single CREATE_EXTENSION change, got %+v", changes)
	}
	if changes[0].Extension == nil || changes[0].Extension.Name != "pgcrypto" {
		t.Fatalf("extension payload missing: %+v", changes[0].Extension)
	}
	if changes[0].Destructive {
		t.Errorf("extension create must not be destructive")
	}
}

func TestDiff_ExtensionNoDiffWhenPresent(t *testing.T) {
	live := &schema.Schema{
		Name:       "public",
		Extensions: map[string]*schema.Extension{"pgcrypto": {Name: "pgcrypto"}},
	}
	desired := &schema.Schema{
		Name:       "public",
		Extensions: map[string]*schema.Extension{"pgcrypto": {Name: "pgcrypto"}},
	}
	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %+v", changes)
	}
}

func TestDiff_ExtensionLiveOnlyNeverDropped(t *testing.T) {
	// Live has an extension the desired SQL does not declare: no drop must be
	// proposed regardless of management state (extensions may be owned by
	// other tools; removal is an explicit operator action).
	live := &schema.Schema{
		Name:       "public",
		Extensions: map[string]*schema.Extension{"orphan_ext": {Name: "orphan_ext"}},
	}
	desired := &schema.Schema{Name: "public", Extensions: map[string]*schema.Extension{}}
	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	for _, c := range changes {
		if c.Type == plan.ChangeDropExtension {
			t.Fatalf("live-only extension must never be auto-dropped: %+v", c)
		}
	}
}
