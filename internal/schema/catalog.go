package schema

import (
	"fmt"
	"regexp"
	"strings"
)

// Publication represents a managed logical-replication publication declared
// in CatalogSQL. Publications are cluster-catalog objects: they never belong
// in SchemaSQL (no shadow-compile story) and are diffed against the live
// pg_publication state.
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

// EventTrigger represents a managed DDL event trigger declared in
// CatalogSQL. Its function must exist (a managed routine from SchemaSQL or
// an operator object); the sync refuses to create the trigger otherwise.
type EventTrigger struct {
	Name  string   `json:"name"`
	Event string   `json:"event"`          // e.g. ddl_command_start, ddl_command_end, sql_drop, table_rewrite
	Tags  []string `json:"tags,omitempty"` // WHEN TAG IN (...) filter; empty = all tags
	// Function is the event-trigger function name (unqualified).
	Function string `json:"function"`
	// Enabled is the desired enabled state (desired-state files cannot
	// carry ENABLE/DISABLE statements; drift renders ALTER EVENT TRIGGER
	// ENABLE/DISABLE).
	Enabled bool `json:"enabled"`
}

// CatalogSpec is the desired publication/event-trigger state parsed from a
// CatalogSQL file.
type CatalogSpec struct {
	Publications  []*Publication  `json:"publications"`
	EventTriggers []*EventTrigger `json:"event_triggers"`
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
	createEventTriggerRe = regexp.MustCompile(`(?is)^CREATE\s+EVENT\s+TRIGGER\s+("([^"]+)"|[\w]+)\s+ON\s+([\w]+)(?:\s+(WHEN\b.*?))?\s+EXECUTE\s+(?:FUNCTION|PROCEDURE)\s+([\w"]+)`)
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
				spec.Publications = append(spec.Publications, p)
			}
		case strings.HasPrefix(upper, "CREATE EVENT TRIGGER"):
			if e := parseEventTriggerStatement(trimmed); e != nil {
				spec.EventTriggers = append(spec.EventTriggers, e)
			}
		}
	}
	return spec
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
			p.PublishInsert, p.PublishUpdate, p.PublishDelete, p.PublishTruncate = false, false, false, false
			for op := range strings.SplitSeq(pm[1], ",") {
				switch strings.ToUpper(strings.TrimSpace(op)) {
				case "INSERT":
					p.PublishInsert = true
				case "UPDATE":
					p.PublishUpdate = true
				case "DELETE":
					p.PublishDelete = true
				case "TRUNCATE":
					p.PublishTruncate = true
				}
			}
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

// ValidateCatalogSQL enforces the CatalogSQL statement contract: only CREATE
// PUBLICATION and CREATE EVENT TRIGGER statements are accepted. Anything
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
		case strings.HasPrefix(upper, "CREATE EVENT TRIGGER"):
			if createEventTriggerRe.FindStringSubmatch(trimmed) == nil {
				return fmt.Errorf("unsupported CREATE EVENT TRIGGER form in CatalogSQL (expected CREATE EVENT TRIGGER <name> ON <event> [WHEN TAG IN ('...')] EXECUTE FUNCTION <fn>()): %q", trimmed)
			}
			if strings.Contains(strings.ToUpper(trimmed), "VALUE IN") {
				return fmt.Errorf("unsupported WHEN VALUE IN filter in CatalogSQL (only WHEN TAG IN is managed): %q", trimmed)
			}
		default:
			return fmt.Errorf("unsupported statement in CatalogSQL (only CREATE PUBLICATION and CREATE EVENT TRIGGER are managed): %q", trimmed)
		}
	}
	return nil
}
