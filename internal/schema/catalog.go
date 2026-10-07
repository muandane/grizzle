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

	droppedPublications  map[string]bool
	droppedEventTriggers map[string]bool
}

var (
	// createPublicationRe matches CREATE PUBLICATION name [FOR ...] [WITH (...)].
	// Captures: (1) publication name, (2) the FOR clause if present,
	// (3) the WITH clause if present.
	createPublicationRe = regexp.MustCompile(`(?is)^CREATE\s+PUBLICATION\s+("([^"]+)"|[\w]+)(?:\s+(FOR\b[^;]+?))?(?:\s+(WITH\s*\(.*\)))?\s*$`)

	// publicationForAllRe matches the FOR ALL TABLES membership clause.
	publicationForAllRe = regexp.MustCompile(`(?i)^ALL\s+TABLES\s*$`)

	// publicationForTablesRe matches "TABLE t1, t2" / "TABLES IN SCHEMA s1, s2".
	publicationForTablesRe = regexp.MustCompile(`(?i)^TABLES\s+IN\s+SCHEMA\s+(.+)$|^TABLE\s+(.+)$`)

	// publishOptionsRe extracts the publish option value from WITH (...).
	publishOptionsRe = regexp.MustCompile(`(?i)publish\s*=\s*'([^']*)'`)

	// createEventTriggerRe matches
	// CREATE EVENT TRIGGER name ON event [WHEN TAG IN (...)] EXECUTE FUNCTION f().
	// Captures: (1) name, (2) event, (3) optional WHEN clause, (4) function.
	createEventTriggerRe = regexp.MustCompile(`(?is)^CREATE\s+EVENT\s+TRIGGER\s+("([^"]+)"|[\w]+)\s+ON\s+([\w]+)(?:\s+(WHEN\b.*?))?\s+EXECUTE\s+(?:FUNCTION|PROCEDURE)\s+([\w"]+)\s*\(\s*\)\s*$`)

	alterPublicationRe        = regexp.MustCompile(`(?is)^ALTER\s+PUBLICATION\s+("([^"]+)"|[\w]+)\s+(.+)$`)
	dropPublicationRe         = regexp.MustCompile(`(?is)^DROP\s+PUBLICATION\s+(?:IF\s+EXISTS\s+)?("([^"]+)"|[\w]+)(?:\s+(?:CASCADE|RESTRICT))?$`)
	alterEventTriggerRe       = regexp.MustCompile(`(?is)^ALTER\s+EVENT\s+TRIGGER\s+("([^"]+)"|[\w]+)\s+(.+)$`)
	dropEventTriggerRe        = regexp.MustCompile(`(?is)^DROP\s+EVENT\s+TRIGGER\s+(?:IF\s+EXISTS\s+)?("([^"]+)"|[\w]+)(?:\s+(?:CASCADE|RESTRICT))?$`)
	publicationPublishAlterRe = regexp.MustCompile(`(?is)^SET\s*\(\s*publish\s*=\s*'[^']*'\s*\)$`)
)

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
				upsertPublication(spec, p)
			}
		case strings.HasPrefix(upper, "ALTER PUBLICATION"):
			applyPublicationAlter(spec, trimmed)
		case strings.HasPrefix(upper, "DROP PUBLICATION"):
			if name := statementIdentifier(dropPublicationRe.FindStringSubmatch(trimmed)); name != "" {
				if spec.droppedPublications == nil {
					spec.droppedPublications = make(map[string]bool)
				}
				spec.droppedPublications[strings.ToLower(name)] = true
				removePublication(spec, name)
			}
		case strings.HasPrefix(upper, "CREATE EVENT TRIGGER"):
			if e := parseEventTriggerStatement(trimmed); e != nil {
				upsertEventTrigger(spec, e)
			}
		case strings.HasPrefix(upper, "ALTER EVENT TRIGGER"):
			applyEventTriggerAlter(spec, trimmed)
		case strings.HasPrefix(upper, "DROP EVENT TRIGGER"):
			if name := statementIdentifier(dropEventTriggerRe.FindStringSubmatch(trimmed)); name != "" {
				if spec.droppedEventTriggers == nil {
					spec.droppedEventTriggers = make(map[string]bool)
				}
				spec.droppedEventTriggers[strings.ToLower(name)] = true
				removeEventTrigger(spec, name)
			}
		}
	}
	return spec
}

func statementIdentifier(matches []string) string {
	if len(matches) < 2 {
		return ""
	}
	if matches[2] != "" {
		return matches[2]
	}
	return strings.Trim(matches[1], `"`)
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

func applyPublicationAlter(spec *CatalogSpec, stmt string) {
	matches := alterPublicationRe.FindStringSubmatch(stmt)
	if matches == nil {
		return
	}
	publication := findPublication(spec, statementIdentifier(matches))
	if publication == nil {
		return
	}
	clause := strings.TrimSpace(matches[3])
	upper := strings.ToUpper(clause)
	switch {
	case strings.HasPrefix(upper, "SET ALL TABLES"):
		publication.AllTables = true
		publication.Tables = nil
		publication.Schemas = nil
	case strings.HasPrefix(upper, "SET TABLES IN SCHEMA "):
		publication.AllTables = false
		publication.Tables = nil
		publication.Schemas = splitList(strings.TrimSpace(clause[len("SET TABLES IN SCHEMA "):]))
	case strings.HasPrefix(upper, "SET TABLE "):
		publication.AllTables = false
		publication.Schemas = nil
		publication.Tables = splitList(strings.TrimSpace(clause[len("SET TABLE "):]))
	case strings.HasPrefix(upper, "ADD TABLES IN SCHEMA "):
		publication.Schemas = appendUniqueFold(publication.Schemas, splitList(strings.TrimSpace(clause[len("ADD TABLES IN SCHEMA "):]))...)
	case strings.HasPrefix(upper, "DROP TABLES IN SCHEMA "):
		publication.Schemas = removeFold(publication.Schemas, splitList(strings.TrimSpace(clause[len("DROP TABLES IN SCHEMA "):]))...)
	case strings.HasPrefix(upper, "ADD TABLE "):
		publication.Tables = appendUniqueFold(publication.Tables, splitList(strings.TrimSpace(clause[len("ADD TABLE "):]))...)
	case strings.HasPrefix(upper, "DROP TABLE "):
		publication.Tables = removeFold(publication.Tables, splitList(strings.TrimSpace(clause[len("DROP TABLE "):]))...)
	case strings.HasPrefix(upper, "SET "):
		if publish := publishOptionsRe.FindStringSubmatch(clause); publish != nil {
			setPublishOptions(publication, publish[1])
		}
	}
}

func applyEventTriggerAlter(spec *CatalogSpec, stmt string) {
	matches := alterEventTriggerRe.FindStringSubmatch(stmt)
	if matches == nil {
		return
	}
	trigger := findEventTrigger(spec, statementIdentifier(matches))
	if trigger == nil {
		return
	}
	switch strings.ToUpper(strings.TrimSpace(matches[3])) {
	case "ENABLE":
		trigger.Enabled = true
	case "DISABLE":
		trigger.Enabled = false
	}
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
	out := &CatalogSpec{}
	publications := make(map[string]*Publication)
	eventTriggers := make(map[string]*EventTrigger)

	for _, spec := range []*CatalogSpec{schemaSpec, sideSpec} {
		if spec == nil {
			continue
		}
		for name := range spec.droppedPublications {
			delete(publications, name)
		}
		for name := range spec.droppedEventTriggers {
			delete(eventTriggers, name)
		}
		for _, publication := range spec.Publications {
			publications[strings.ToLower(publication.Name)] = clonePublication(publication)
		}
		for _, trigger := range spec.EventTriggers {
			eventTriggers[strings.ToLower(trigger.Name)] = cloneEventTrigger(trigger)
		}
	}

	for _, publication := range publications {
		out.Publications = append(out.Publications, publication)
	}
	for _, trigger := range eventTriggers {
		out.EventTriggers = append(out.EventTriggers, trigger)
	}
	slices.SortFunc(out.Publications, func(a, b *Publication) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	slices.SortFunc(out.EventTriggers, func(a, b *EventTrigger) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
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
		Name:            strings.Trim(m[1], `"`),
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
		Name:     strings.Trim(m[1], `"`),
		Event:    strings.ToUpper(m[3]),
		Function: strings.Trim(m[5], `"`),
		Enabled:  true,
	}
	if whenClause := strings.TrimSpace(m[4]); whenClause != "" {
		// WHEN TAG IN ('a', 'b') — extract the quoted tag list. Only the
		// TAG filter is supported (VALUE IN is not diffable via catalogs
		// and is rejected by ValidateCatalogSQL).
		tags := regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(whenClause, -1)
		for _, tm := range tags {
			e.Tags = append(e.Tags, tm[1])
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
			if createPublicationRe.FindStringSubmatch(trimmed) == nil {
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
			if createEventTriggerRe.FindStringSubmatch(trimmed) == nil {
				return fmt.Errorf("unsupported CREATE EVENT TRIGGER form in CatalogSQL (expected CREATE EVENT TRIGGER <name> ON <event> [WHEN TAG IN ('...')] EXECUTE FUNCTION <fn>()): %q", trimmed)
			}
			if strings.Contains(strings.ToUpper(trimmed), "VALUE IN") {
				return fmt.Errorf("unsupported WHEN VALUE IN filter in CatalogSQL (only WHEN TAG IN is managed): %q", trimmed)
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

func isSupportedPublicationAlter(clause string) bool {
	clause = strings.TrimSpace(clause)
	upper := strings.ToUpper(clause)
	switch {
	case upper == "SET ALL TABLES":
		return true
	case strings.HasPrefix(upper, "SET TABLES IN SCHEMA "):
		return strings.TrimSpace(clause[len("SET TABLES IN SCHEMA "):]) != ""
	case strings.HasPrefix(upper, "SET TABLE "):
		return strings.TrimSpace(clause[len("SET TABLE "):]) != ""
	case strings.HasPrefix(upper, "ADD TABLES IN SCHEMA "):
		return strings.TrimSpace(clause[len("ADD TABLES IN SCHEMA "):]) != ""
	case strings.HasPrefix(upper, "DROP TABLES IN SCHEMA "):
		return strings.TrimSpace(clause[len("DROP TABLES IN SCHEMA "):]) != ""
	case strings.HasPrefix(upper, "ADD TABLE "):
		return strings.TrimSpace(clause[len("ADD TABLE "):]) != ""
	case strings.HasPrefix(upper, "DROP TABLE "):
		return strings.TrimSpace(clause[len("DROP TABLE "):]) != ""
	default:
		return publicationPublishAlterRe.MatchString(clause)
	}
}

func isSupportedEventTriggerAlter(clause string) bool {
	switch strings.ToUpper(strings.TrimSpace(clause)) {
	case "ENABLE", "DISABLE":
		return true
	default:
		return false
	}
}
