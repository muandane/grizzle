package diff

import (
	"testing"

	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func TestCatalogDiff_SubscriptionCreateAlterDrop(t *testing.T) {
	desired := &schema.CatalogSpec{
		Subscriptions: []*schema.Subscription{{
			Name:         "sub_a",
			ConnInfo:     "host=db password=s3cret",
			Publications: []string{"pub1"},
			Enabled:      true,
			CopyData:     true,
			SlotName:     "sub_a",
		}},
	}
	live := &CatalogLiveState{
		Publications:     map[string]*PublicationState{},
		EventTriggers:    map[string]*EventTriggerState{},
		Subscriptions:    map[string]*SubscriptionState{},
		ReplicationSlots: map[string]*ReplicationSlotState{},
	}
	changes := CatalogDiff(desired, live, "public")
	if len(changes) != 1 || changes[0].Type != plan.ChangeCreateSubscription {
		t.Fatalf("expected CREATE_SUBSCRIPTION, got %+v", changes)
	}

	live.Subscriptions["sub_a"] = &SubscriptionState{
		Name:         "sub_a",
		Managed:      true,
		ConnInfo:     "host=db password=old",
		Publications: []string{"pub1"},
		Enabled:      false,
		SlotName:     "sub_a",
	}
	changes = CatalogDiff(desired, live, "public")
	if len(changes) != 1 || changes[0].Type != plan.ChangeAlterSubscription {
		t.Fatalf("expected ALTER_SUBSCRIPTION, got %+v", changes)
	}

	desiredEmpty := &schema.CatalogSpec{}
	desiredEmpty.Subscriptions = nil
	// Simulate managed live-only drop via empty desired with create history suppressed
	// by leaving a managed live subscription and no suppress flags.
	changes = CatalogDiff(&schema.CatalogSpec{}, live, "public")
	if len(changes) != 1 || changes[0].Type != plan.ChangeDropSubscription || !changes[0].Destructive {
		t.Fatalf("expected DROP_SUBSCRIPTION for managed live-only, got %+v", changes)
	}
}

func TestCatalogDiff_ReplicationSlotCreateAndExplicitDrop(t *testing.T) {
	desired := schema.ParseCatalogSQL(`
SELECT pg_create_logical_replication_slot('slot_a', 'pgoutput');
SELECT pg_drop_replication_slot('slot_b');
`)
	live := &CatalogLiveState{
		Publications:  map[string]*PublicationState{},
		EventTriggers: map[string]*EventTriggerState{},
		Subscriptions: map[string]*SubscriptionState{},
		ReplicationSlots: map[string]*ReplicationSlotState{
			"slot_b":        {Name: "slot_b", Plugin: "pgoutput"},
			"operator_slot": {Name: "operator_slot", Plugin: "pgoutput"},
		},
	}
	changes := CatalogDiff(desired, live, "public")
	var sawCreate, sawDrop, sawOperatorDrop bool
	for _, c := range changes {
		switch c.Type {
		case plan.ChangeCreateReplicationSlot:
			if c.Table == "slot_a" {
				sawCreate = true
			}
		case plan.ChangeDropReplicationSlot:
			if c.Table == "slot_b" {
				sawDrop = true
			}
			if c.Table == "operator_slot" {
				sawOperatorDrop = true
			}
		}
	}
	if !sawCreate {
		t.Fatal("expected CREATE_REPLICATION_SLOT for slot_a")
	}
	if !sawDrop {
		t.Fatal("expected DROP_REPLICATION_SLOT for explicit slot_b")
	}
	if sawOperatorDrop {
		t.Fatal("live-only operator slots must never be swept")
	}
}

func TestCatalogDiff_SubscriptionOwnedSlotIgnored(t *testing.T) {
	desired := &schema.CatalogSpec{
		Subscriptions: []*schema.Subscription{{
			Name:         "sub_a",
			ConnInfo:     "host=x",
			Publications: []string{"p"},
			Enabled:      true,
			CopyData:     true,
			SlotName:     "sub_a",
		}},
		ReplicationSlots: []*schema.ReplicationSlot{{
			Name:   "sub_a",
			Plugin: "pgoutput",
		}},
	}
	live := &CatalogLiveState{
		Publications:  map[string]*PublicationState{},
		EventTriggers: map[string]*EventTriggerState{},
		Subscriptions: map[string]*SubscriptionState{},
		ReplicationSlots: map[string]*ReplicationSlotState{
			"sub_a": {Name: "sub_a", Plugin: "pgoutput", OwnedBySubscription: true},
		},
	}
	changes := CatalogDiff(desired, live, "public")
	for _, c := range changes {
		if c.Type == plan.ChangeCreateReplicationSlot || c.Type == plan.ChangeDropReplicationSlot {
			t.Fatalf("subscription-owned slots must not be managed separately: %+v", c)
		}
	}
}
