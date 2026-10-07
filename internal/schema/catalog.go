package schema

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Publication represents a managed logical-replication publication declared
// in SchemaSQL or CatalogSQL. Publications are cluster-catalog objects: they
// bypass shadow compilation and are diffed against live pg_publication state.
type Publication struct {
	Name string `json:"name"`
	// AllTables mirrors FOR ALL TABLES (every table in the database,
	// including future ones).
	AllTables bool `json:"all_tables"`
	// Tables lists individually published tables (schema-qualified where
	// declared; canonicalized to schema-qualified lowercase by the diff).
	Tables []string `json:"tables,omitempty"`
	// Schemas lists schemas whose tables are published wholesale
	// (FOR TABLES IN SCHEMA ...).
	Schemas []string `json:"schemas,omitempty"`

	PublishInsert   bool `json:"publish_insert"`
	PublishUpdate   bool `json:"publish_update"`
	PublishDelete   bool `json:"publish_delete"`
	PublishTruncate bool `json:"publish_truncate"`
}

// EventTrigger represents a managed DDL event trigger declared in SchemaSQL
// or CatalogSQL. Its function must exist (a managed routine from SchemaSQL or
// an operator object); the sync refuses to create the trigger otherwise.
type EventTrigger struct {
	Name  string   `json:"name"`
	Event string   `json:"event"`          // e.g. ddl_command_start, ddl_command_end, sql_drop, table_rewrite
	Tags  []string `json:"tags,omitempty"` // WHEN TAG IN (...) filter; empty = all tags
	// Function is the event-trigger function name (unqualified).
	Function string `json:"function"`
	// Enabled is the desired enabled state; ALTER EVENT TRIGGER
	// ENABLE/DISABLE statements may override it in the unified input.
	Enabled bool `json:"enabled"`
}

// CatalogSpec is the desired publication/event-trigger state parsed from a
// unified schema or side-channel SQL file.
type CatalogSpec struct {
	Publications  []*Publication  `json:"publications"`
	EventTriggers []*EventTrigger `json:"event_triggers"`

	droppedPublications   map[string]bool
	droppedEventTriggers  map[string]bool
	operations            []catalogOperation
	hasCatalogStatements  bool
	hasCreateStatements   bool
	explicitDropsOnly     bool
	suppressImplicitDrops bool
}

type catalogOperationKind uint8

const (
	catalogPublicationOperation catalogOperationKind = iota
	catalogEventTriggerOperation
)

type catalogOperationAction uint8

const (
	catalogCreateOperation catalogOperationAction = iota
	catalogAlterOperation
	catalogDropOperation
)

type catalogOperation struct {
	kind         catalogOperationKind
	action       catalogOperationAction
	name         string
	publication  *Publication
	eventTrigger *EventTrigger
	alterClause  string
}

var (
	// catalogIdentifierPattern captures a SQL identifier while preserving
	// escaped double quotes in quoted identifiers.
	catalogIdentifierPartPattern = `(?:"(?:""|[^"])*"|[A-Za-z_][A-Za-z0-9_$]*)`
	catalogIdentifierPattern     = `("((?:""|[^"])*)"|[A-Za-z_][A-Za-z0-9_$]*)`
	catalogIdentifierPartRe      = regexp.MustCompile(`^` + catalogIdentifierPartPattern + `$`)

	// createPublicationRe matches CREATE PUBLICATION name [FOR ...] [WITH (...)].
	// Captures: (1) publication name, (2) quoted-name contents when quoted,
	// (3) the FOR clause if present, (4) the WITH clause if present.
	createPublicationRe = regexp.MustCompile(`(?is)^CREATE\s+PUBLICATION\s+` + catalogIdentifierPattern + `(?:\s+(FOR\b.*?))?(?:\s+(WITH\s*\(.*\)))?\s*$`)

	// publicationForAllRe matches the FOR ALL TABLES membership clause.
	publicationForAllRe = regexp.MustCompile(`(?i)^ALL\s+TABLES\s*$`)

	// publicationForTablesRe matches "TABLE t1, t2" / "TABLES IN SCHEMA s1, s2".
	publicationForTablesRe = regexp.MustCompile(`(?i)^TABLES\s+IN\s+SCHEMA\s+(.+)$|^TABLE\s+(.+)$`)

	// publishOptionsRe extracts the publish option value from WITH (...).
	publishOptionsRe  = regexp.MustCompile(`(?i)publish\s*=\s*'([^']*)'`)
	publicationWithRe = regexp.MustCompile(`(?is)^WITH\s*\(\s*publish\s*=\s*'([^']*)'\s*\)$`)

	// createEventTriggerRe matches
	// CREATE EVENT TRIGGER name ON event [WHEN TAG IN (...)] EXECUTE FUNCTION f().
	// Captures: (1) name, (2) quoted-name contents when quoted, (3) event,
	// (4) optional WHEN clause, (5) function.
	createEventTriggerRe = regexp.MustCompile(`(?is)^CREATE\s+EVENT\s+TRIGGER\s+` + catalogIdentifierPattern + `\s+ON\s+([A-Za-z_][A-Za-z0-9_$]*)(?:\s+(WHEN\b.*?))?\s+EXECUTE\s+(?:FUNCTION|PROCEDURE)\s+(` + catalogIdentifierPartPattern + `)\s*\(\s*\)\s*$`)

	alterPublicationRe        = regexp.MustCompile(`(?is)^ALTER\s+PUBLICATION\s+` + catalogIdentifierPattern + `\s+(.+)$`)
	dropPublicationRe         = regexp.MustCompile(`(?is)^DROP\s+PUBLICATION\s+(?:IF\s+EXISTS\s+)?` + catalogIdentifierPattern + `(?:\s+(?:CASCADE|RESTRICT))?$`)
	alterEventTriggerRe       = regexp.MustCompile(`(?is)^ALTER\s+EVENT\s+TRIGGER\s+` + catalogIdentifierPattern + `\s+(.+)$`)
	dropEventTriggerRe        = regexp.MustCompile(`(?is)^DROP\s+EVENT\s+TRIGGER\s+(?:IF\s+EXISTS\s+)?` + catalogIdentifierPattern + `(?:\s+(?:CASCADE|RESTRICT))?$`)
	publicationSetAllRe       = regexp.MustCompile(`(?is)^SET\s+ALL\s+TABLES$`)
	publicationSetSchemasRe   = regexp.MustCompile(`(?is)^SET\s+TABLES\s+IN\s+SCHEMA\s+(.+)$`)
	publicationSetTablesRe    = regexp.MustCompile(`(?is)^SET\s+TABLE\s+(.+)$`)
	publicationAddSchemasRe   = regexp.MustCompile(`(?is)^ADD\s+TABLES\s+IN\s+SCHEMA\s+(.+)$`)
	publicationDropSchemasRe  = regexp.MustCompile(`(?is)^DROP\s+TABLES\s+IN\s+SCHEMA\s+(.+)$`)
	publicationAddTablesRe    = regexp.MustCompile(`(?is)^ADD\s+TABLE\s+(.+)$`)
	publicationDropTablesRe   = regexp.MustCompile(`(?is)^DROP\s+TABLE\s+(.+)$`)
	publicationPublishAlterRe = regexp.MustCompile(`(?is)^SET\s*\(\s*publish\s*=\s*'([^']*)'\s*\)$`)
	eventTriggerWhenTagRe     = regexp.MustCompile(`(?is)^WHEN\s+TAG\s+IN\s*\(\s*'(?:''|[^'])*'(?:\s*,\s*'(?:''|[^'])*')*\s*\)$`)
	eventTriggerTagValueRe    = regexp.MustCompile(`'((?:''|[^'])*)'`)
)

var supportedEventTriggerEvents = map[string]bool{
	"DDL_COMMAND_START": true,
	"DDL_COMMAND_END":   true,
	"SQL_DROP":          true,
	"TABLE_REWRITE":     true,
}

// ParseCatalogSQL extracts managed publications and event triggers from a
// CatalogSQL file. CREATE PUBLICATION without a WITH (publish = ...) clause
// adopts the PostgreSQL defaults (all four DML operations). CREATE EVENT
// TRIGGER without a WHEN TAG clause matches all tags; parsed triggers are
// desired-enabled.
func ParseCatalogSQL(sql string) *CatalogSpec {
	spec := &CatalogSpec{}
	for _, stmt := range SplitStatements(sql) {
		trimmed := stripLeadingComments(stmt)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "CREATE PUBLICATION"):
			if p := parsePublicationStatement(trimmed); p != nil {
				spec.operations = append(spec.operations, catalogOperation{
					kind:        catalogPublicationOperation,
					action:      catalogCreateOperation,
					name:        p.Name,
					publication: p,
				})
				spec.hasCatalogStatements = true
				spec.hasCreateStatements = true
			}
		case strings.HasPrefix(upper, "ALTER PUBLICATION"):
			if matches := alterPublicationRe.FindStringSubmatch(trimmed); matches != nil {
				spec.operations = append(spec.operations, catalogOperation{
					kind:        catalogPublicationOperation,
					action:      catalogAlterOperation,
					name:        statementIdentifier(matches),
					alterClause: strings.TrimSpace(matches[3]),
				})
				spec.hasCatalogStatements = true
			}
		case strings.HasPrefix(upper, "DROP PUBLICATION"):
			if name := statementIdentifier(dropPublicationRe.FindStringSubmatch(trimmed)); name != "" {
				spec.operations = append(spec.operations, catalogOperation{
					kind:   catalogPublicationOperation,
					action: catalogDropOperation,
					name:   name,
				})
				spec.hasCatalogStatements = true
			}
		case strings.HasPrefix(upper, "CREATE EVENT TRIGGER"):
			if e := parseEventTriggerStatement(trimmed); e != nil {
				spec.operations = append(spec.operations, catalogOperation{
					kind:         catalogEventTriggerOperation,
					action:       catalogCreateOperation,
					name:         e.Name,
					eventTrigger: e,
				})
				spec.hasCatalogStatements = true
				spec.hasCreateStatements = true
			}
		case strings.HasPrefix(upper, "ALTER EVENT TRIGGER"):
			if matches := alterEventTriggerRe.FindStringSubmatch(trimmed); matches != nil {
				spec.operations = append(spec.operations, catalogOperation{
					kind:        catalogEventTriggerOperation,
					action:      catalogAlterOperation,
					name:        statementIdentifier(matches),
					alterClause: strings.TrimSpace(matches[3]),
				})
				spec.hasCatalogStatements = true
			}
		case strings.HasPrefix(upper, "DROP EVENT TRIGGER"):
			if name := statementIdentifier(dropEventTriggerRe.FindStringSubmatch(trimmed)); name != "" {
				spec.operations = append(spec.operations, catalogOperation{
					kind:   catalogEventTriggerOperation,
					action: catalogDropOperation,
					name:   name,
				})
				spec.hasCatalogStatements = true
			}
		}
	}
	materialized, _ := applyCatalogOperations(nil, spec.operations, false)
	materialized.operations = append([]catalogOperation(nil), spec.operations...)
	materialized.hasCatalogStatements = spec.hasCatalogStatements
	materialized.hasCreateStatements = spec.hasCreateStatements
	materialized.suppressImplicitDrops = spec.hasCatalogStatements && !spec.hasCreateStatements
	return materialized
}

func statementIdentifier(matches []string) string {
	if len(matches) < 2 {
		return ""
	}
	if len(matches) > 2 && matches[2] != "" {
		return strings.ReplaceAll(matches[2], `""`, `"`)
	}
	return decodeIdentifier(matches[1])
}

func decodeIdentifier(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if len(identifier) >= 2 && identifier[0] == '"' && identifier[len(identifier)-1] == '"' {
		return strings.ReplaceAll(identifier[1:len(identifier)-1], `""`, `"`)
	}
	return strings.ToLower(identifier)
}

func upsertPublication(spec *CatalogSpec, publication *Publication) {
	key := strings.ToLower(publication.Name)
	for i, existing := range spec.Publications {
		if strings.ToLower(existing.Name) == key {
			spec.Publications[i] = publication
			return
		}
	}
	spec.Publications = append(spec.Publications, publication)
}

func removePublication(spec *CatalogSpec, name string) {
	filtered := spec.Publications[:0]
	for _, publication := range spec.Publications {
		if !strings.EqualFold(publication.Name, name) {
			filtered = append(filtered, publication)
		}
	}
	spec.Publications = filtered
}

func findPublication(spec *CatalogSpec, name string) *Publication {
	for _, publication := range spec.Publications {
		if strings.EqualFold(publication.Name, name) {
			return publication
		}
	}
	return nil
}

func upsertEventTrigger(spec *CatalogSpec, trigger *EventTrigger) {
	key := strings.ToLower(trigger.Name)
	for i, existing := range spec.EventTriggers {
		if strings.ToLower(existing.Name) == key {
			spec.EventTriggers[i] = trigger
			return
		}
	}
	spec.EventTriggers = append(spec.EventTriggers, trigger)
}

func removeEventTrigger(spec *CatalogSpec, name string) {
	filtered := spec.EventTriggers[:0]
	for _, trigger := range spec.EventTriggers {
		if !strings.EqualFold(trigger.Name, name) {
			filtered = append(filtered, trigger)
		}
	}
	spec.EventTriggers = filtered
}

func findEventTrigger(spec *CatalogSpec, name string) *EventTrigger {
	for _, trigger := range spec.EventTriggers {
		if strings.EqualFold(trigger.Name, name) {
			return trigger
		}
	}
	return nil
}

func applyCatalogOperations(base *CatalogSpec, operations []catalogOperation, strict bool) (*CatalogSpec, error) {
	out := &CatalogSpec{}
	if base != nil {
		for _, publication := range base.Publications {
			upsertPublication(out, clonePublication(publication))
		}
		for _, trigger := range base.EventTriggers {
			upsertEventTrigger(out, cloneEventTrigger(trigger))
		}
		out.droppedPublications = cloneBoolMap(base.droppedPublications)
		out.droppedEventTriggers = cloneBoolMap(base.droppedEventTriggers)
	}
	for _, operation := range operations {
		switch operation.kind {
		case catalogPublicationOperation:
			switch operation.action {
			case catalogCreateOperation:
				upsertPublication(out, clonePublication(operation.publication))
				delete(out.droppedPublications, strings.ToLower(operation.name))
			case catalogAlterOperation:
				publication := findPublication(out, operation.name)
				if publication == nil {
					if strict {
						return nil, fmt.Errorf("ALTER PUBLICATION %q requires a CREATE PUBLICATION declaration in SchemaSQL or CatalogSQL", operation.name)
					}
					continue
				}
				if !applyPublicationAlterClause(publication, operation.alterClause) && strict {
					return nil, fmt.Errorf("unsupported ALTER PUBLICATION form for %q: %q", operation.name, operation.alterClause)
				}
			case catalogDropOperation:
				removePublication(out, operation.name)
				if out.droppedPublications == nil {
					out.droppedPublications = make(map[string]bool)
				}
				out.droppedPublications[strings.ToLower(operation.name)] = true
			}
		case catalogEventTriggerOperation:
			switch operation.action {
			case catalogCreateOperation:
				upsertEventTrigger(out, cloneEventTrigger(operation.eventTrigger))
				delete(out.droppedEventTriggers, strings.ToLower(operation.name))
			case catalogAlterOperation:
				trigger := findEventTrigger(out, operation.name)
				if trigger == nil {
					if strict {
						return nil, fmt.Errorf("ALTER EVENT TRIGGER %q requires a CREATE EVENT TRIGGER declaration in SchemaSQL or CatalogSQL", operation.name)
					}
					continue
				}
				if !applyEventTriggerAlterClause(trigger, operation.alterClause) && strict {
					return nil, fmt.Errorf("unsupported ALTER EVENT TRIGGER form for %q: %q", operation.name, operation.alterClause)
				}
			case catalogDropOperation:
				removeEventTrigger(out, operation.name)
				if out.droppedEventTriggers == nil {
					out.droppedEventTriggers = make(map[string]bool)
				}
				out.droppedEventTriggers[strings.ToLower(operation.name)] = true
			}
		}
	}
	return out, nil
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func applyPublicationAlterClause(publication *Publication, clause string) bool {
	clause = strings.TrimSpace(clause)
	if publicationSetAllRe.MatchString(clause) {
		publication.AllTables = true
		publication.Tables = nil
		publication.Schemas = nil
		return true
	}
	if matches := publicationSetSchemasRe.FindStringSubmatch(clause); matches != nil {
		publication.AllTables = false
		publication.Tables = nil
		publication.Schemas = splitList(matches[1])
		return true
	}
	if matches := publicationSetTablesRe.FindStringSubmatch(clause); matches != nil {
		publication.AllTables = false
		publication.Schemas = nil
		publication.Tables = splitList(matches[1])
		return true
	}
	if matches := publicationAddSchemasRe.FindStringSubmatch(clause); matches != nil {
		publication.Schemas = appendUniqueFold(publication.Schemas, splitList(matches[1])...)
		return true
	}
	if matches := publicationDropSchemasRe.FindStringSubmatch(clause); matches != nil {
		publication.Schemas = removeFold(publication.Schemas, splitList(matches[1])...)
		return true
	}
	if matches := publicationAddTablesRe.FindStringSubmatch(clause); matches != nil {
		publication.Tables = appendUniqueFold(publication.Tables, splitList(matches[1])...)
		return true
	}
	if matches := publicationDropTablesRe.FindStringSubmatch(clause); matches != nil {
		publication.Tables = removeFold(publication.Tables, splitList(matches[1])...)
		return true
	}
	if matches := publicationPublishAlterRe.FindStringSubmatch(clause); matches != nil {
		setPublishOptions(publication, matches[1])
		return true
	}
	return false
}

func applyEventTriggerAlterClause(trigger *EventTrigger, clause string) bool {
	switch strings.ToUpper(strings.TrimSpace(clause)) {
	case "ENABLE":
		trigger.Enabled = true
	case "DISABLE":
		trigger.Enabled = false
	default:
		return false
	}
	return true
}

func setPublishOptions(publication *Publication, options string) {
	publication.PublishInsert = false
	publication.PublishUpdate = false
	publication.PublishDelete = false
	publication.PublishTruncate = false
	for op := range strings.SplitSeq(options, ",") {
		switch strings.ToUpper(strings.TrimSpace(op)) {
		case "INSERT":
			publication.PublishInsert = true
		case "UPDATE":
			publication.PublishUpdate = true
		case "DELETE":
			publication.PublishDelete = true
		case "TRUNCATE":
			publication.PublishTruncate = true
		}
	}
}

func appendUniqueFold(values []string, additions ...string) []string {
	for _, addition := range additions {
		found := false
		for _, value := range values {
			if strings.EqualFold(value, addition) {
				found = true
				break
			}
		}
		if !found {
			values = append(values, addition)
		}
	}
	return values
}

func removeFold(values []string, removals ...string) []string {
	filtered := values[:0]
	for _, value := range values {
		remove := false
		for _, candidate := range removals {
			if strings.EqualFold(value, candidate) {
				remove = true
				break
			}
		}
		if !remove {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

// MergeCatalogSpecs combines catalog desired state from SchemaSQL with the
// optional CatalogSQL side-channel. The explicit side-channel replaces
// SchemaSQL publications or event triggers with the same name.
func MergeCatalogSpecs(schemaSpec, sideSpec *CatalogSpec) *CatalogSpec {
	out, _ := mergeCatalogSpecs(schemaSpec, sideSpec, false)
	return out
}

// ValidateCatalogSpecMerge verifies that ALTER statements have a declaration
// to modify after SchemaSQL and CatalogSQL overlays are applied.
func ValidateCatalogSpecMerge(schemaSpec, sideSpec *CatalogSpec) error {
	_, err := mergeCatalogSpecs(schemaSpec, sideSpec, true)
	return err
}

func mergeCatalogSpecs(schemaSpec, sideSpec *CatalogSpec, strict bool) (*CatalogSpec, error) {
	out, err := materializeCatalogSpec(schemaSpec, strict)
	if err != nil {
		return nil, err
	}
	if sideSpec != nil {
		if len(sideSpec.operations) > 0 {
			out, err = applyCatalogOperations(out, sideSpec.operations, strict)
			if err != nil {
				return nil, err
			}
		} else {
			overlayCatalogDeclarations(out, sideSpec)
		}
	}
	out.hasCatalogStatements = hasCatalogStatements(schemaSpec) || hasCatalogStatements(sideSpec)
	out.hasCreateStatements = hasCreateStatements(schemaSpec) || hasCreateStatements(sideSpec)
	out.explicitDropsOnly = out.hasCatalogStatements &&
		(len(out.Publications) == 0 && len(out.EventTriggers) == 0) &&
		(len(out.droppedPublications) > 0 || len(out.droppedEventTriggers) > 0)
	out.suppressImplicitDrops = suppressImplicitDrops(schemaSpec) || suppressImplicitDrops(sideSpec)
	slices.SortFunc(out.Publications, func(a, b *Publication) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	slices.SortFunc(out.EventTriggers, func(a, b *EventTrigger) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

func materializeCatalogSpec(spec *CatalogSpec, strict bool) (*CatalogSpec, error) {
	if spec == nil {
		return &CatalogSpec{}, nil
	}
	if len(spec.operations) > 0 {
		out, err := applyCatalogOperations(nil, spec.operations, strict)
		if err != nil {
			return nil, err
		}
		out.operations = append([]catalogOperation(nil), spec.operations...)
		return out, nil
	}
	return cloneCatalogSpec(spec), nil
}

func overlayCatalogDeclarations(out, overlay *CatalogSpec) {
	for name := range overlay.droppedPublications {
		deletePublication(out, name)
	}
	for name := range overlay.droppedEventTriggers {
		deleteEventTrigger(out, name)
	}
	for _, publication := range overlay.Publications {
		upsertPublication(out, clonePublication(publication))
		delete(out.droppedPublications, strings.ToLower(publication.Name))
	}
	for _, trigger := range overlay.EventTriggers {
		upsertEventTrigger(out, cloneEventTrigger(trigger))
		delete(out.droppedEventTriggers, strings.ToLower(trigger.Name))
	}
}

func deletePublication(spec *CatalogSpec, name string) {
	removePublication(spec, name)
	if spec.droppedPublications == nil {
		spec.droppedPublications = make(map[string]bool)
	}
	spec.droppedPublications[strings.ToLower(name)] = true
}

func deleteEventTrigger(spec *CatalogSpec, name string) {
	removeEventTrigger(spec, name)
	if spec.droppedEventTriggers == nil {
		spec.droppedEventTriggers = make(map[string]bool)
	}
	spec.droppedEventTriggers[strings.ToLower(name)] = true
}

func cloneCatalogSpec(spec *CatalogSpec) *CatalogSpec {
	out := &CatalogSpec{
		hasCatalogStatements:  spec.hasCatalogStatements,
		hasCreateStatements:   spec.hasCreateStatements,
		explicitDropsOnly:     spec.explicitDropsOnly,
		suppressImplicitDrops: spec.suppressImplicitDrops,
		operations:            append([]catalogOperation(nil), spec.operations...),
		droppedPublications:   cloneBoolMap(spec.droppedPublications),
		droppedEventTriggers:  cloneBoolMap(spec.droppedEventTriggers),
	}
	for _, publication := range spec.Publications {
		upsertPublication(out, clonePublication(publication))
	}
	for _, trigger := range spec.EventTriggers {
		upsertEventTrigger(out, cloneEventTrigger(trigger))
	}
	return out
}

func hasCatalogStatements(spec *CatalogSpec) bool {
	return spec != nil && spec.hasCatalogStatements
}

func hasCreateStatements(spec *CatalogSpec) bool {
	return spec != nil && spec.hasCreateStatements
}

func suppressImplicitDrops(spec *CatalogSpec) bool {
	return spec != nil && spec.suppressImplicitDrops
}

func clonePublication(p *Publication) *Publication {
	if p == nil {
		return nil
	}
	out := *p
	out.Tables = append([]string(nil), p.Tables...)
	out.Schemas = append([]string(nil), p.Schemas...)
	return &out
}

func cloneEventTrigger(e *EventTrigger) *EventTrigger {
	if e == nil {
		return nil
	}
	out := *e
	out.Tags = append([]string(nil), e.Tags...)
	return &out
}

// parsePublicationStatement expands one CREATE PUBLICATION statement.
func parsePublicationStatement(stmt string) *Publication {
	m := createPublicationRe.FindStringSubmatch(stmt)
	if m == nil {
		return nil
	}
	p := &Publication{
		Name:            statementIdentifier(m),
		PublishInsert:   true,
		PublishUpdate:   true,
		PublishDelete:   true,
		PublishTruncate: true,
	}
	if forClause := strings.TrimSpace(m[3]); forClause != "" {
		// The captured clause includes the leading FOR keyword; strip it so
		// the content regexes match against "ALL TABLES" / "TABLE ..." /
		// "TABLES IN SCHEMA ...".
		clause := forClause
		if len(clause) >= 3 && strings.EqualFold(clause[:3], "FOR") {
			clause = strings.TrimSpace(clause[3:])
		}
		forAll := publicationForAllRe.FindStringSubmatch(clause)
		forTables := publicationForTablesRe.FindStringSubmatch(clause)
		switch {
		case forAll != nil:
			p.AllTables = true
		case forTables != nil:
			// The regex alternation routes "TABLES IN SCHEMA s1, s2" to
			// capture group 1 and "TABLE t1, t2" to group 2.
			list := forTables[1]
			if list == "" {
				list = forTables[2]
			}
			if forTables[1] != "" {
				p.Schemas = splitList(list)
			} else {
				p.Tables = splitList(list)
			}
		}
	}
	if withClause := strings.TrimSpace(m[4]); withClause != "" {
		if pm := publishOptionsRe.FindStringSubmatch(withClause); pm != nil {
			setPublishOptions(p, pm[1])
		}
	}
	return p
}

// parseEventTriggerStatement expands one CREATE EVENT TRIGGER statement.
func parseEventTriggerStatement(stmt string) *EventTrigger {
	m := createEventTriggerRe.FindStringSubmatch(stmt)
	if m == nil {
		return nil
	}
	e := &EventTrigger{
		Name:     statementIdentifier(m),
		Event:    strings.ToUpper(m[3]),
		Function: decodeIdentifier(m[5]),
		Enabled:  true,
	}
	if whenClause := strings.TrimSpace(m[4]); whenClause != "" {
		// WHEN TAG IN ('a', 'b') — extract the quoted tag list. Validation
		// rejects all other WHEN forms before this parser is used by sync.
		for _, tm := range eventTriggerTagValueRe.FindAllStringSubmatch(whenClause, -1) {
			e.Tags = append(e.Tags, strings.ReplaceAll(tm[1], "''", "'"))
		}
	}
	return e
}

// PublicationManagedComment is the catalog comment marker Grizzle stamps on
// publications it created. Only marker-stamped publications absent from the
// desired state are considered for dropping.
const PublicationManagedComment = "grizzle-managed"

// EventTriggerManagedComment is the catalog comment marker Grizzle stamps on
// event triggers it created.
const EventTriggerManagedComment = "grizzle-managed"

// PublicationExplicitlyDropped reports whether the desired catalog input
// contains a DROP PUBLICATION for name.
func (s *CatalogSpec) PublicationExplicitlyDropped(name string) bool {
	return s != nil && s.droppedPublications[strings.ToLower(name)]
}

// EventTriggerExplicitlyDropped reports whether the desired catalog input
// contains a DROP EVENT TRIGGER for name.
func (s *CatalogSpec) EventTriggerExplicitlyDropped(name string) bool {
	return s != nil && s.droppedEventTriggers[strings.ToLower(name)]
}

// ExplicitDropsOnly reports whether the input contains only explicit drops
// and no remaining catalog declarations. Such input must not sweep every
// managed catalog object as an implicit desired-state omission.
func (s *CatalogSpec) ExplicitDropsOnly() bool {
	return s != nil && s.explicitDropsOnly
}

// SuppressImplicitDrops reports whether this input contains only ALTER/DROP
// operations. Such a patch requires an existing declaration and must not
// sweep unrelated managed catalog objects that were omitted from the patch.
func (s *CatalogSpec) SuppressImplicitDrops() bool {
	return s != nil && s.suppressImplicitDrops
}

// ValidateCatalogSQL enforces the CatalogSQL statement contract. CREATE,
// ALTER, and DROP publication/event-trigger statements are accepted; anything
// else fails loudly instead of being silently ignored by the statement scan.
func ValidateCatalogSQL(sql string) error {
	for _, stmt := range SplitStatements(sql) {
		trimmed := stripLeadingComments(stmt)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "CREATE PUBLICATION"):
			matches := createPublicationRe.FindStringSubmatch(trimmed)
			if matches == nil || !validatePublicationCreate(matches) {
				return fmt.Errorf("unsupported CREATE PUBLICATION form in CatalogSQL (expected CREATE PUBLICATION <name> [FOR ALL TABLES | FOR TABLE <tables> | FOR TABLES IN SCHEMA <schemas>] [WITH (publish = '...')]): %q", trimmed)
			}
		case strings.HasPrefix(upper, "ALTER PUBLICATION"):
			matches := alterPublicationRe.FindStringSubmatch(trimmed)
			if matches == nil || !isSupportedPublicationAlter(matches[3]) {
				return fmt.Errorf("unsupported ALTER PUBLICATION form in CatalogSQL: %q", trimmed)
			}
		case strings.HasPrefix(upper, "DROP PUBLICATION"):
			if dropPublicationRe.FindStringSubmatch(trimmed) == nil {
				return fmt.Errorf("unsupported DROP PUBLICATION form in CatalogSQL: %q", trimmed)
			}
		case strings.HasPrefix(upper, "CREATE EVENT TRIGGER"):
			matches := createEventTriggerRe.FindStringSubmatch(trimmed)
			if matches == nil {
				return fmt.Errorf("unsupported CREATE EVENT TRIGGER form in CatalogSQL (expected CREATE EVENT TRIGGER <name> ON <event> [WHEN TAG IN ('...')] EXECUTE FUNCTION <fn>()): %q", trimmed)
			}
			if !supportedEventTriggerEvents[strings.ToUpper(matches[3])] {
				return fmt.Errorf("unsupported event-trigger event %q in CatalogSQL: %q", matches[3], trimmed)
			}
			if whenClause := strings.TrimSpace(matches[4]); whenClause != "" && !eventTriggerWhenTagRe.MatchString(whenClause) {
				return fmt.Errorf("unsupported event-trigger WHEN clause in CatalogSQL (only WHEN TAG IN is managed): %q", trimmed)
			}
		case strings.HasPrefix(upper, "ALTER EVENT TRIGGER"):
			matches := alterEventTriggerRe.FindStringSubmatch(trimmed)
			if matches == nil || !isSupportedEventTriggerAlter(matches[3]) {
				return fmt.Errorf("unsupported ALTER EVENT TRIGGER form in CatalogSQL: %q", trimmed)
			}
		case strings.HasPrefix(upper, "DROP EVENT TRIGGER"):
			if dropEventTriggerRe.FindStringSubmatch(trimmed) == nil {
				return fmt.Errorf("unsupported DROP EVENT TRIGGER form in CatalogSQL: %q", trimmed)
			}
		default:
			return fmt.Errorf("unsupported statement in CatalogSQL (only CREATE/ALTER/DROP PUBLICATION and EVENT TRIGGER are managed): %q", trimmed)
		}
	}
	return nil
}

func validatePublicationCreate(matches []string) bool {
	if len(matches) < 5 {
		return false
	}
	if forClause := strings.TrimSpace(matches[3]); forClause != "" {
		if containsSQLCommentOutsideQuotes(forClause) {
			return false
		}
		clause := strings.TrimSpace(forClause[3:])
		if publicationForAllRe.MatchString(clause) {
			// Valid.
		} else if tableMatches := publicationForTablesRe.FindStringSubmatch(clause); tableMatches != nil {
			list := tableMatches[1]
			qualified := true
			if list == "" {
				list = tableMatches[2]
				qualified = true
			}
			if tableMatches[1] != "" {
				qualified = false
			}
			if !nonEmptyIdentifierList(list, qualified) {
				return false
			}
		} else {
			return false
		}
	}
	if withClause := strings.TrimSpace(matches[4]); withClause != "" {
		if containsSQLCommentOutsideQuotes(withClause) {
			return false
		}
		withMatches := publicationWithRe.FindStringSubmatch(withClause)
		if withMatches == nil || validatePublishOptions(withMatches[1]) != nil {
			return false
		}
	}
	return true
}

func containsSQLCommentOutsideQuotes(s string) bool {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if inQuote && i+1 < len(s) && s[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
		case '-':
			if !inQuote && i+1 < len(s) && s[i+1] == '-' {
				return true
			}
		case '/':
			if !inQuote && i+1 < len(s) && s[i+1] == '*' {
				return true
			}
		}
	}
	return false
}

func validatePublishOptions(options string) error {
	seen := make(map[string]bool)
	for _, option := range strings.Split(options, ",") {
		option = strings.ToLower(strings.TrimSpace(option))
		switch option {
		case "insert", "update", "delete", "truncate":
			if seen[option] {
				return fmt.Errorf("duplicate publish option %q", option)
			}
			seen[option] = true
		default:
			return fmt.Errorf("unsupported publish option %q", option)
		}
	}
	if len(seen) == 0 {
		return fmt.Errorf("publish option list must not be empty")
	}
	return nil
}

func isSupportedPublicationAlter(clause string) bool {
	clause = strings.TrimSpace(clause)
	if containsSQLCommentOutsideQuotes(clause) {
		return false
	}
	switch {
	case publicationSetAllRe.MatchString(clause):
		return true
	case publicationSetSchemasRe.MatchString(clause):
		return nonEmptyIdentifierList(publicationSetSchemasRe.FindStringSubmatch(clause)[1], false)
	case publicationSetTablesRe.MatchString(clause):
		return nonEmptyIdentifierList(publicationSetTablesRe.FindStringSubmatch(clause)[1], true)
	case publicationAddSchemasRe.MatchString(clause):
		return nonEmptyIdentifierList(publicationAddSchemasRe.FindStringSubmatch(clause)[1], false)
	case publicationDropSchemasRe.MatchString(clause):
		return nonEmptyIdentifierList(publicationDropSchemasRe.FindStringSubmatch(clause)[1], false)
	case publicationAddTablesRe.MatchString(clause):
		return nonEmptyIdentifierList(publicationAddTablesRe.FindStringSubmatch(clause)[1], true)
	case publicationDropTablesRe.MatchString(clause):
		return nonEmptyIdentifierList(publicationDropTablesRe.FindStringSubmatch(clause)[1], true)
	case publicationPublishAlterRe.MatchString(clause):
		matches := publicationPublishAlterRe.FindStringSubmatch(clause)
		return validatePublishOptions(matches[1]) == nil
	default:
		return false
	}
}

func nonEmptyIdentifierList(list string, qualified bool) bool {
	items, ok := splitIdentifierList(list)
	if !ok || len(items) == 0 {
		return false
	}
	maxParts := 1
	if qualified {
		maxParts = 2
	}
	for _, item := range items {
		parts := splitQualifiedIdentifier(item)
		if len(parts) == 0 || len(parts) > maxParts {
			return false
		}
		for _, part := range parts {
			if !catalogIdentifierPartRe.MatchString(strings.TrimSpace(part)) {
				return false
			}
		}
	}
	return true
}

func splitIdentifierList(list string) ([]string, bool) {
	var parts []string
	start := 0
	inQuote := false
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '"':
			if inQuote && i+1 < len(list) && list[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
		case ',':
			if inQuote {
				continue
			}
			item := strings.TrimSpace(list[start:i])
			if item == "" {
				return nil, false
			}
			parts = append(parts, item)
			start = i + 1
		}
	}
	if inQuote {
		return nil, false
	}
	item := strings.TrimSpace(list[start:])
	if item == "" {
		return nil, false
	}
	parts = append(parts, item)
	return parts, true
}

func splitQualifiedIdentifier(name string) []string {
	var parts []string
	start := 0
	inQuote := false
	for i := 0; i < len(name); i++ {
		switch name[i] {
		case '"':
			if inQuote && i+1 < len(name) && name[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
		case '.':
			if !inQuote {
				parts = append(parts, strings.TrimSpace(name[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(name[start:]))
	return parts
}

func isSupportedEventTriggerAlter(clause string) bool {
	switch strings.ToUpper(strings.TrimSpace(clause)) {
	case "ENABLE", "DISABLE":
		return true
	default:
		return false
	}
}
