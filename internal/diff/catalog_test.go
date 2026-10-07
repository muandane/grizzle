package diff

import (
	"testing"

	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func TestCatalogDiff_CreateAndNoOp(t *testing.T) {
	desired := &schema.CatalogSpec{
		Publications: []*schema.Publication{
			{Name: "docs_pub", Tables: []string{"docs"}, PublishInsert: true, PublishUpdate: true, PublishDelete: true, PublishTruncate: true},
		},
		EventTriggers: []*schema.EventTrigger{
			{Name: "audit", Event: "ddl_command_end", Function: "log_ddl", Enabled: true},
		},
	}
	live := &CatalogLiveState{
		Publications: map[string]*PublicationState{
			"docs_pub": {
				Name: "docs_pub", Managed: true,
				Tables:        []string{"public.docs"},
				PublishInsert: true, PublishUpdate: true, PublishDelete: true, PublishTruncate: true,
			},
		},
		EventTriggers: map[string]*EventTriggerState{
			"audit": {Name: "audit", Managed: true, Event: "DDL_COMMAND_END", Function: "log_ddl", Enabled: true},
		},
	}

	changes := CatalogDiff(desired, live, "public")
	if len(changes) != 0 {
		t.Fatalf("in-sync state should produce no changes, got %d: %+v", len(changes), changes)
	}

	// Greenfield: everything missing.
	empty := &CatalogLiveState{
		Publications:  map[string]*PublicationState{},
		EventTriggers: map[string]*EventTriggerState{},
	}
	changes = CatalogDiff(desired, empty, "public")
	if len(changes) != 2 {
		t.Fatalf("greenfield should produce 2 changes, got %d", len(changes))
	}
	if changes[0].Type != plan.ChangeCreatePublication || changes[1].Type != plan.ChangeCreateEventTrigger {
		t.Errorf("unexpected change types: %s, %s", changes[0].Type, changes[1].Type)
	}
}

func TestCatalogDiff_PublicationDrift(t *testing.T) {
	desired := &schema.CatalogSpec{
		Publications: []*schema.Publication{
			{
				Name: "docs_pub", Tables: []string{"docs", "comments"},
				PublishInsert: true, PublishUpdate: false, PublishDelete: false, PublishTruncate: false,
			},
		},
	}
	live := &CatalogLiveState{
		Publications: map[string]*PublicationState{
			"docs_pub": {
				Name: "docs_pub", Managed: true,
				Tables:        []string{"public.docs"},
				PublishInsert: true, PublishUpdate: true, PublishDelete: true, PublishTruncate: true,
			},
		},
		EventTriggers: map[string]*EventTriggerState{},
	}

	changes := CatalogDiff(desired, live, "public")
	if len(changes) != 1 || changes[0].Type != plan.ChangeAlterPublication {
		t.Fatalf("want single ALTER_PUBLICATION, got %+v", changes)
	}
	c := changes[0]
	if c.Publication == nil || c.OldPublication == nil {
		t.Fatal("ALTER_PUBLICATION must carry desired and old publication IR")
	}
	// Unqualified desired table "comments" canonicalized against target schema.
	found := false
	for _, tbl := range c.Publication.Tables {
		if tbl == "public.comments" {
			found = true
		}
	}
	if !found {
		t.Errorf("desired tables should be canonicalized to target schema, got %v", c.Publication.Tables)
	}
}

func TestCatalogDiff_EventTriggerDrift(t *testing.T) {
	t.Run("definition drift recreates", func(t *testing.T) {
		desired := &schema.CatalogSpec{
			EventTriggers: []*schema.EventTrigger{
				{Name: "audit", Event: "ddl_command_end", Function: "log_ddl_v2", Enabled: true},
			},
		}
		live := &CatalogLiveState{
			EventTriggers: map[string]*EventTriggerState{
				"audit": {Name: "audit", Managed: true, Event: "DDL_COMMAND_END", Function: "log_ddl", Enabled: true},
			},
		}
		changes := CatalogDiff(desired, live, "public")
		if len(changes) != 1 || changes[0].Type != plan.ChangeAlterEventTrigger {
			t.Fatalf("want single ALTER_EVENT_TRIGGER, got %+v", changes)
		}
		if changes[0].EventTrigger == nil || changes[0].OldEventTrigger == nil {
			t.Fatal("ALTER_EVENT_TRIGGER must carry desired and old trigger IR")
		}
	})

	t.Run("enabled drift alters", func(t *testing.T) {
		desired := &schema.CatalogSpec{
			EventTriggers: []*schema.EventTrigger{
				{Name: "audit", Event: "ddl_command_end", Function: "log_ddl", Enabled: false},
			},
		}
		live := &CatalogLiveState{
			EventTriggers: map[string]*EventTriggerState{
				"audit": {Name: "audit", Managed: true, Event: "DDL_COMMAND_END", Function: "log_ddl", Enabled: true},
			},
		}
		changes := CatalogDiff(desired, live, "public")
		if len(changes) != 1 || changes[0].Type != plan.ChangeAlterEventTrigger {
			t.Fatalf("want single ALTER_EVENT_TRIGGER, got %+v", changes)
		}
	})

	t.Run("tag drift recreates", func(t *testing.T) {
		desired := &schema.CatalogSpec{
			EventTriggers: []*schema.EventTrigger{
				{Name: "audit", Event: "ddl_command_end", Tags: []string{"CREATE TABLE"}, Function: "log_ddl", Enabled: true},
			},
		}
		live := &CatalogLiveState{
			EventTriggers: map[string]*EventTriggerState{
				"audit": {Name: "audit", Managed: true, Event: "DDL_COMMAND_END", Function: "log_ddl", Enabled: true},
			},
		}
		changes := CatalogDiff(desired, live, "public")
		if len(changes) != 1 || changes[0].Type != plan.ChangeAlterEventTrigger {
			t.Fatalf("want single ALTER_EVENT_TRIGGER, got %+v", changes)
		}
	})
}

func TestCatalogDiff_NarrowDrops(t *testing.T) {
	t.Run("managed live-only dropped", func(t *testing.T) {
		live := &CatalogLiveState{
			Publications: map[string]*PublicationState{
				"stale_pub": {Name: "stale_pub", Managed: true},
			},
			EventTriggers: map[string]*EventTriggerState{
				"stale_trig": {Name: "stale_trig", Managed: true, Event: "SQL_DROP", Function: "fn", Enabled: true},
			},
		}
		desired := &schema.CatalogSpec{
			Publications: []*schema.Publication{
				{Name: "docs_pub", Tables: []string{"docs"}, PublishInsert: true, PublishUpdate: true, PublishDelete: true, PublishTruncate: true},
			},
		}
		changes := CatalogDiff(desired, live, "public")
		var dropPub, dropTrig bool
		for _, c := range changes {
			switch c.Type {
			case plan.ChangeDropPublication:
				dropPub = true
				if !c.Destructive {
					t.Error("DROP_PUBLICATION must be destructive")
				}
			case plan.ChangeDropEventTrigger:
				dropTrig = true
				if !c.Destructive {
					t.Error("DROP_EVENT_TRIGGER must be destructive")
				}
			case plan.ChangeCreatePublication:
			default:
				t.Errorf("unexpected change type %s", c.Type)
			}
		}
		if !dropPub || !dropTrig {
			t.Errorf("want drops for both stale objects, got pub=%v trig=%v", dropPub, dropTrig)
		}
	})

	t.Run("unmanaged live-only never swept", func(t *testing.T) {
		live := &CatalogLiveState{
			Publications: map[string]*PublicationState{
				"operator_pub": {Name: "operator_pub", Managed: false},
			},
			EventTriggers: map[string]*EventTriggerState{
				"operator_trig": {Name: "operator_trig", Managed: false, Event: "SQL_DROP", Function: "fn", Enabled: true},
			},
		}
		changes := CatalogDiff(&schema.CatalogSpec{}, live, "public")
		if len(changes) != 0 {
			t.Fatalf("unmanaged live-only objects must never be dropped, got %+v", changes)
		}
	})
}
