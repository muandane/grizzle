package diff

import (
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

// CatalogLiveState is the live publication/event-trigger state inspected
// from the database catalogs.
type CatalogLiveState struct {
	Publications  map[string]*PublicationState
	EventTriggers map[string]*EventTriggerState
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

// CatalogDiff computes publication and event-trigger sync steps from the
// desired CatalogSQL state and the live catalog state.
//
// Destructive drops are narrow: live-only publications and event triggers
// are dropped only when they carry the grizzle-managed marker comment
// (operator-created catalog objects are never swept). Publications and
// event triggers named in the desired state are always managed.
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

	return changes
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
