package diff

import (
	"fmt"
	"sort"
	"strings"

	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// PublicationState is the dialect-independent live publication snapshot
// consumed by CatalogDiff.
type PublicationState struct {
	Name string
	// Managed marks a live publication stamped with the grizzle-managed
	// marker comment. Only managed live-only publications are droppable.
	Managed bool

	AllTables bool
	Tables    []string // canonical schema-qualified identities
	Schemas   []string // canonical identifiers

	PublishInsert   bool
	PublishUpdate   bool
	PublishDelete   bool
	PublishTruncate bool
}

// EventTriggerState is the dialect-independent live event-trigger snapshot
// consumed by CatalogDiff.
type EventTriggerState struct {
	Name string
	// Managed marks a live event trigger stamped with the grizzle-managed
	// marker comment.
	Managed bool

	Event string
	Tags  []string
	// Function is the canonical schema-qualified routine identity, including
	// identity arguments when inspected from PostgreSQL.
	Function string
	Enabled  bool
}

// SubscriptionState is the dialect-independent live subscription snapshot
// consumed by CatalogDiff.
type SubscriptionState struct {
	Name string
	// Managed marks a live subscription stamped with the grizzle-managed
	// marker comment. Only managed live-only subscriptions are droppable.
	Managed bool

	ConnInfo     string
	SlotName     string
	Publications []string
	Enabled      bool
}

// ReplicationSlotState is the dialect-independent live logical slot snapshot.
// Replication slots do not support COMMENT ON; Managed is unused for sweep
// decisions — live-only slots are never auto-dropped.
type ReplicationSlotState struct {
	Name      string
	Plugin    string
	Temporary bool
	// ActivePID is non-nil when a backend holds the slot; drops are refused.
	ActivePID *int
	// OwnedBySubscription is true when this slot name matches a live
	// subscription's subslotname; such slots are not managed separately.
	OwnedBySubscription bool
}

// CatalogLiveState is the live publication/event-trigger/subscription/slot
// state inspected from the database catalogs.
type CatalogLiveState struct {
	Publications     map[string]*PublicationState
	EventTriggers    map[string]*EventTriggerState
	Subscriptions    map[string]*SubscriptionState
	ReplicationSlots map[string]*ReplicationSlotState
}

// canonicalNameList lowercases, trims, and deduplicates a name slice,
// sorting for stable comparison.
func canonicalNameList(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		l := strings.ToLower(strings.TrimSpace(s))
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// sameStringSet compares two canonical name lists for set equality.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// canonicalizePublication returns the desired publication with canonical
// names: unqualified tables resolve against targetSchema so desired state
// written against the working schema matches the schema-qualified names
// reported by catalog introspection.
func canonicalizePublication(p *schema.Publication, targetSchema string) *schema.Publication {
	tables := make([]string, 0, len(p.Tables))
	for _, t := range p.Tables {
		if strings.TrimSpace(t) == "" {
			continue
		}
		tables = append(tables, canonicalQualifiedIdentifier(t, targetSchema))
	}
	cp := *p
	cp.Tables = canonicalQualifiedList(tables)
	cp.Schemas = canonicalSchemaList(p.Schemas)
	return &cp
}

func canonicalSchemaList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, name := range in {
		parts := schema.ParseQualifiedIdentifier(name)
		if len(parts) != 1 {
			continue
		}
		name = schema.CanonicalIdentifierPart(parts[0])
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func canonicalQualifiedList(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func canonicalQualifiedIdentifier(name, targetSchema string) string {
	return schema.CanonicalQualifiedIdentifier(name, targetSchema)
}

func canonicalIdentifierPart(part string) string {
	return schema.CanonicalIdentifierPart(part)
}

func splitQualifiedIdentifier(name string) []string {
	return schema.ParseQualifiedIdentifier(name)
}

// CatalogDiff computes publication, event-trigger, subscription, and
// logical-slot sync steps from the desired CatalogSQL state and the live
// catalog state.
//
// Destructive drops are narrow: publications, event triggers, and
// subscriptions are dropped only when they carry the grizzle-managed marker
// comment — including the recreate (DROP+CREATE) path used when definition
// drift cannot be expressed as ALTER. Operator-created catalog objects without
// the marker are never swept. Standalone logical slots never auto-sweep
// (COMMENT ON unsupported); drops require an explicit pg_drop_replication_slot.
// Objects named in the desired state are managed for create/alter; drops still
// require the marker.
//
// targetSchema qualifies unqualified table names in desired publications.
func CatalogDiff(desired *schema.CatalogSpec, live *CatalogLiveState, targetSchema string) []Change {
	var changes []Change

	// --- Publications ---
	liveOnlyPubs := make(map[string]bool, len(live.Publications))
	for name := range live.Publications {
		liveOnlyPubs[name] = true
	}

	for _, p := range desired.Publications {
		key := schema.CanonicalIdentifierKey(p.Name)
		liveP, exists := live.Publications[key]
		liveOnlyPubs[key] = false
		want := canonicalizePublication(p, targetSchema)
		if !exists {
			changes = append(changes, Change{
				Type:        plan.ChangeCreatePublication,
				Table:       want.Name,
				Publication: want,
			})
			continue
		}
		if publicationNeedsRecreate(want, liveP) {
			if !liveP.Managed {
				// Operator-created publication occupies this name. Never DROP
				// it; callers must refuse via RefuseUnmanagedCatalogRecreates
				// so Sync fails closed instead of silently drifting.
				continue
			}
			changes = append(changes,
				Change{
					Type:        plan.ChangeDropPublication,
					Table:       liveP.Name,
					Destructive: true,
					Publication: &schema.Publication{Name: liveP.Name},
				},
				Change{
					Type:        plan.ChangeCreatePublication,
					Table:       want.Name,
					Publication: want,
				},
			)
			continue
		}
		if publicationHasDrift(want, liveP) {
			changes = append(changes, Change{
				Type:        plan.ChangeAlterPublication,
				Table:       want.Name,
				Destructive: publicationIsNarrowing(want, liveP),
				Publication: want,
				OldPublication: &schema.Publication{
					Name:            liveP.Name,
					AllTables:       liveP.AllTables,
					Tables:          liveP.Tables,
					Schemas:         liveP.Schemas,
					PublishInsert:   liveP.PublishInsert,
					PublishUpdate:   liveP.PublishUpdate,
					PublishDelete:   liveP.PublishDelete,
					PublishTruncate: liveP.PublishTruncate,
				},
			})
		}
	}

	// Live-only publications: dropped only when marker-stamped.
	for _, name := range sortedMapKeysLivePubs(live.Publications) {
		if desired.PublicationExplicitlyDropped(name) {
			liveOnlyPubs[name] = false
			if live.Publications[name].Managed {
				changes = append(changes, Change{
					Type:        plan.ChangeDropPublication,
					Table:       name,
					Destructive: true,
					Publication: &schema.Publication{Name: name},
				})
			}
			continue
		}
		if desired.ExplicitDropsOnly() {
			continue
		}
		if desired.SuppressImplicitDrops() {
			continue
		}
		if !liveOnlyPubs[name] || !live.Publications[name].Managed {
			continue
		}
		changes = append(changes, Change{
			Type:        plan.ChangeDropPublication,
			Table:       name,
			Destructive: true,
			Publication: &schema.Publication{Name: name},
		})
	}

	// --- Event triggers ---
	liveOnlyETs := make(map[string]bool, len(live.EventTriggers))
	for name := range live.EventTriggers {
		liveOnlyETs[name] = true
	}

	for _, e := range desired.EventTriggers {
		key := schema.CanonicalIdentifierKey(e.Name)
		liveE, exists := live.EventTriggers[key]
		liveOnlyETs[key] = false
		if !exists {
			changes = append(changes, Change{
				Type:         plan.ChangeCreateEventTrigger,
				Table:        e.Name,
				EventTrigger: e,
			})
			continue
		}
		if eventTriggerNeedsRecreate(e, liveE, targetSchema) {
			if !liveE.Managed {
				// Operator-created event trigger occupies this name. Never
				// DROP it; callers must refuse via RefuseUnmanagedCatalogRecreates.
				continue
			}
			changes = append(changes,
				Change{
					Type:         plan.ChangeDropEventTrigger,
					Table:        liveE.Name,
					Destructive:  true,
					EventTrigger: &schema.EventTrigger{Name: liveE.Name},
				},
				Change{
					Type:         plan.ChangeCreateEventTrigger,
					Table:        e.Name,
					EventTrigger: e,
				},
			)
		} else if e.Enabled != liveE.Enabled {
			changes = append(changes, Change{
				Type:         plan.ChangeAlterEventTrigger,
				Table:        e.Name,
				EventTrigger: e,
				OldEventTrigger: &schema.EventTrigger{
					Name:     liveE.Name,
					Event:    liveE.Event,
					Tags:     append([]string(nil), liveE.Tags...),
					Function: liveE.Function,
					Enabled:  liveE.Enabled,
				},
			})
		}
	}

	for _, name := range sortedMapKeysLiveETs(live.EventTriggers) {
		if desired.EventTriggerExplicitlyDropped(name) {
			liveOnlyETs[name] = false
			if live.EventTriggers[name].Managed {
				changes = append(changes, Change{
					Type:         plan.ChangeDropEventTrigger,
					Table:        name,
					Destructive:  true,
					EventTrigger: &schema.EventTrigger{Name: name},
				})
			}
			continue
		}
		if desired.ExplicitDropsOnly() {
			continue
		}
		if desired.SuppressImplicitDrops() {
			continue
		}
		if !liveOnlyETs[name] || !live.EventTriggers[name].Managed {
			continue
		}
		changes = append(changes, Change{
			Type:         plan.ChangeDropEventTrigger,
			Table:        name,
			Destructive:  true,
			EventTrigger: &schema.EventTrigger{Name: name},
		})
	}

	// --- Subscriptions ---
	liveOnlySubs := make(map[string]bool, len(live.Subscriptions))
	for name := range live.Subscriptions {
		liveOnlySubs[name] = true
	}

	for _, s := range desired.Subscriptions {
		key := schema.CanonicalIdentifierKey(s.Name)
		liveS, exists := live.Subscriptions[key]
		liveOnlySubs[key] = false
		want := canonicalizeSubscription(s)
		if !exists {
			changes = append(changes, Change{
				Type:         plan.ChangeCreateSubscription,
				Table:        want.Name,
				Subscription: want,
			})
			continue
		}
		if subscriptionHasDrift(want, liveS) {
			changes = append(changes, Change{
				Type:         plan.ChangeAlterSubscription,
				Table:        want.Name,
				Subscription: want,
				OldSubscription: &schema.Subscription{
					Name:         liveS.Name,
					ConnInfo:     liveS.ConnInfo,
					SlotName:     liveS.SlotName,
					Publications: append([]string(nil), liveS.Publications...),
					Enabled:      liveS.Enabled,
					CopyData:     want.CopyData,
				},
			})
		}
	}

	for _, name := range sortedMapKeysLiveSubs(live.Subscriptions) {
		if desired.SubscriptionExplicitlyDropped(name) {
			liveOnlySubs[name] = false
			if live.Subscriptions[name].Managed {
				changes = append(changes, Change{
					Type:         plan.ChangeDropSubscription,
					Table:        name,
					Destructive:  true,
					Subscription: &schema.Subscription{Name: name},
				})
			}
			continue
		}
		if desired.ExplicitDropsOnly() {
			continue
		}
		if desired.SuppressImplicitDrops() {
			continue
		}
		if !liveOnlySubs[name] || !live.Subscriptions[name].Managed {
			continue
		}
		changes = append(changes, Change{
			Type:         plan.ChangeDropSubscription,
			Table:        name,
			Destructive:  true,
			Subscription: &schema.Subscription{Name: name},
		})
	}

	// --- Standalone logical replication slots ---
	// Slots owned by subscriptions are ignored. Live-only slots are never
	// swept (no COMMENT ON support); drops require an explicit drop statement.
	subscriptionSlotNames := make(map[string]bool)
	for _, sub := range desired.Subscriptions {
		if sub.SlotName != "" {
			subscriptionSlotNames[schema.CanonicalIdentifierKey(sub.SlotName)] = true
		}
	}
	for _, liveSub := range live.Subscriptions {
		if liveSub.SlotName != "" {
			subscriptionSlotNames[schema.CanonicalIdentifierKey(liveSub.SlotName)] = true
		}
	}

	liveOnlySlots := make(map[string]bool, len(live.ReplicationSlots))
	for name, slot := range live.ReplicationSlots {
		if slot.OwnedBySubscription || subscriptionSlotNames[name] {
			continue
		}
		liveOnlySlots[name] = true
	}

	for _, slot := range desired.ReplicationSlots {
		key := schema.CanonicalIdentifierKey(slot.Name)
		if subscriptionSlotNames[key] {
			continue
		}
		liveSlot, exists := live.ReplicationSlots[key]
		liveOnlySlots[key] = false
		if !exists || liveSlot.OwnedBySubscription {
			changes = append(changes, Change{
				Type:            plan.ChangeCreateReplicationSlot,
				Table:           slot.Name,
				ReplicationSlot: slot,
			})
		}
	}

	for _, name := range sortedMapKeysLiveSlots(live.ReplicationSlots) {
		liveSlot := live.ReplicationSlots[name]
		if liveSlot.OwnedBySubscription || subscriptionSlotNames[name] {
			continue
		}
		if !desired.ReplicationSlotExplicitlyDropped(name) {
			continue
		}
		if liveSlot.ActivePID != nil {
			// Diff refuses active slots; apply path also checks.
			continue
		}
		changes = append(changes, Change{
			Type:            plan.ChangeDropReplicationSlot,
			Table:           name,
			Destructive:     true,
			ReplicationSlot: &schema.ReplicationSlot{Name: name, Plugin: liveSlot.Plugin, Temporary: liveSlot.Temporary},
		})
	}

	return changes
}

// RefuseUnmanagedCatalogRecreates fails closed when a desired publication or
// event trigger shares a name with an operator-created live object whose
// definition cannot be reconciled without DROP+CREATE. CatalogDiff never
// emits that DROP (marker-narrow policy); without this check Sync would
// silently leave the desired state unconverged.
func RefuseUnmanagedCatalogRecreates(desired *schema.CatalogSpec, live *CatalogLiveState, targetSchema string) error {
	if desired == nil || live == nil {
		return nil
	}
	for _, p := range desired.Publications {
		if p == nil {
			continue
		}
		key := schema.CanonicalIdentifierKey(p.Name)
		liveP, exists := live.Publications[key]
		if !exists || liveP.Managed {
			continue
		}
		want := canonicalizePublication(p, targetSchema)
		if publicationNeedsRecreate(want, liveP) {
			return fmt.Errorf("refusing to replace publication %q: live object lacks the grizzle-managed marker; drop or rename the operator-created publication, or stamp COMMENT ON PUBLICATION ... IS 'grizzle-managed' before syncing", liveP.Name)
		}
	}
	for _, e := range desired.EventTriggers {
		if e == nil {
			continue
		}
		key := schema.CanonicalIdentifierKey(e.Name)
		liveE, exists := live.EventTriggers[key]
		if !exists || liveE.Managed {
			continue
		}
		if eventTriggerNeedsRecreate(e, liveE, targetSchema) {
			return fmt.Errorf("refusing to replace event trigger %q: live object lacks the grizzle-managed marker; drop or rename the operator-created event trigger, or stamp COMMENT ON EVENT TRIGGER ... IS 'grizzle-managed' before syncing", liveE.Name)
		}
	}
	return nil
}

func canonicalizeSubscription(s *schema.Subscription) *schema.Subscription {
	out := cloneSub(s)
	pubs := make([]string, 0, len(out.Publications))
	seen := make(map[string]bool, len(out.Publications))
	for _, p := range out.Publications {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		pubs = append(pubs, p)
	}
	sort.Strings(pubs)
	out.Publications = pubs
	return out
}

func cloneSub(s *schema.Subscription) *schema.Subscription {
	out := *s
	out.Publications = append([]string(nil), s.Publications...)
	return &out
}

func subscriptionHasDrift(want *schema.Subscription, live *SubscriptionState) bool {
	if want.Enabled != live.Enabled {
		return true
	}
	// A redacted desired conninfo (plan artifact) cannot be compared with the
	// live plaintext; conninfo drift is only evaluated for usable IR. Apply
	// refuses redacted-only CONNECTION, so no unverifiable change is executed.
	if schema.SubscriptionHasUsableConnInfo(want) && want.ConnInfo != live.ConnInfo {
		return true
	}
	if want.SlotName != "" && live.SlotName != "" &&
		schema.CanonicalIdentifierKey(want.SlotName) != schema.CanonicalIdentifierKey(live.SlotName) {
		// Slot rename is not managed via ALTER; treat as non-drift for now
		// (create-time attribute). Conninfo/pubs/enabled are the managed surface.
		_ = want.SlotName
	}
	return !sameStringSet(canonicalNameList(want.Publications), canonicalNameList(live.Publications))
}

func sortedMapKeysLiveSubs(m map[string]*SubscriptionState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedMapKeysLiveSlots(m map[string]*ReplicationSlotState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// publicationHasDrift reports membership or publish-flag drift between the
// desired and live publication states.
func publicationHasDrift(want *schema.Publication, live *PublicationState) bool {
	if want.AllTables != live.AllTables {
		return true
	}
	if want.PublishInsert != live.PublishInsert || want.PublishUpdate != live.PublishUpdate ||
		want.PublishDelete != live.PublishDelete || want.PublishTruncate != live.PublishTruncate {
		return true
	}
	if !sameStringSet(want.Tables, live.Tables) {
		return true
	}
	return !sameStringSet(want.Schemas, live.Schemas)
}

func publicationNeedsRecreate(want *schema.Publication, live *PublicationState) bool {
	return live.AllTables && !want.AllTables &&
		len(want.Tables) == 0 && len(want.Schemas) == 0
}

// publicationIsNarrowing reports replication-surface reductions. PostgreSQL
// applies these with ALTER PUBLICATION, but they can stop rows from reaching
// subscribers and therefore require the publication drop policy/hazard gate.
func publicationIsNarrowing(want *schema.Publication, live *PublicationState) bool {
	if live.AllTables && !want.AllTables {
		return true
	}
	if !want.AllTables && !live.AllTables {
		wantTables := make(map[string]bool, len(want.Tables))
		for _, table := range want.Tables {
			wantTables[table] = true
		}
		for _, table := range live.Tables {
			if !wantTables[table] {
				return true
			}
		}
		wantSchemas := make(map[string]bool, len(want.Schemas))
		for _, schemaName := range want.Schemas {
			wantSchemas[schemaName] = true
		}
		for _, schemaName := range live.Schemas {
			if !wantSchemas[schemaName] {
				return true
			}
		}
	}
	return (live.PublishInsert && !want.PublishInsert) ||
		(live.PublishUpdate && !want.PublishUpdate) ||
		(live.PublishDelete && !want.PublishDelete) ||
		(live.PublishTruncate && !want.PublishTruncate)
}

// eventTriggerNeedsRecreate reports whether the desired event-trigger
// definition differs from the live one in a way that requires DROP+CREATE
// (PostgreSQL has no ALTER EVENT TRIGGER for event/tags/function).
func eventTriggerNeedsRecreate(want *schema.EventTrigger, live *EventTriggerState, targetSchema string) bool {
	if !strings.EqualFold(want.Event, live.Event) ||
		!eventTriggerFunctionsEqual(want.Function, live.Function, targetSchema) {
		return true
	}
	return !sameStringSet(canonicalNameList(want.Tags), canonicalNameList(live.Tags))
}

func eventTriggerFunctionsEqual(want, live, targetSchema string) bool {
	wantCanonical := canonicalEventTriggerFunction(want, targetSchema)
	liveCanonical := canonicalEventTriggerFunction(live, "")
	if wantCanonical == liveCanonical {
		return true
	}

	// Older live snapshots and dialect adapters may provide an unqualified
	// function name. Compare its exact routine component without treating a
	// missing schema as a definition change.
	liveName, liveArgs := splitFunctionIdentity(live)
	liveParts := splitQualifiedIdentifier(liveName)
	if len(liveParts) != 1 {
		return false
	}
	wantName, wantArgs := splitFunctionIdentity(want)
	wantParts := splitQualifiedIdentifier(wantName)
	if len(wantParts) == 0 || strings.TrimSpace(liveArgs) != strings.TrimSpace(wantArgs) {
		return false
	}
	return canonicalIdentifierPart(liveParts[0]) ==
		canonicalIdentifierPart(wantParts[len(wantParts)-1])
}

func canonicalEventTriggerFunction(value, targetSchema string) string {
	name, args := splitFunctionIdentity(value)
	return schema.CanonicalQualifiedIdentifier(name, targetSchema) +
		"(" + strings.TrimSpace(args) + ")"
}

func splitFunctionIdentity(value string) (name, args string) {
	value = strings.TrimSpace(value)
	inQuote := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			if inQuote && i+1 < len(value) && value[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
		case '(':
			if !inQuote {
				name = strings.TrimSpace(value[:i])
				args = strings.TrimSpace(value[i+1:])
				if strings.HasSuffix(args, ")") {
					args = strings.TrimSpace(args[:len(args)-1])
				}
				return name, args
			}
		}
	}
	return value, ""
}

// sortedMapKeysLivePubs returns deterministically ordered publication names.
func sortedMapKeysLivePubs(m map[string]*PublicationState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedMapKeysLiveETs returns deterministically ordered event-trigger names.
func sortedMapKeysLiveETs(m map[string]*EventTriggerState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
