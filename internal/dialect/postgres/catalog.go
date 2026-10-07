package postgres

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/schema"
)

// LiveCatalog captures the publication/event-trigger state relevant to a
// CatalogSQL diff.
type LiveCatalog struct {
	// Publications maps lowercased publication name -> live state.
	Publications map[string]*diff.PublicationState
	// EventTriggers maps lowercased event-trigger name -> live state.
	EventTriggers map[string]*diff.EventTriggerState
}

// InspectLiveCatalog reads publications (with table membership and publish
// flags) and event triggers (with event, tag filter, function, enabled
// state) plus their grizzle-managed marker comments.
func InspectLiveCatalog(ctx context.Context, dbtx dialect.DBTX) (*LiveCatalog, error) {
	live := &LiveCatalog{
		Publications:  make(map[string]*diff.PublicationState),
		EventTriggers: make(map[string]*diff.EventTriggerState),
	}

	// 1. Publications: markers via obj_description (publications are normal
	// (non-shared) catalog objects).
	pubRows, err := dbtx.QueryContext(ctx, `
		SELECT p.pubname,
		       p.puballtables,
		       p.pubinsert,
		       p.pubupdate,
		       p.pubdelete,
		       p.pubtruncate,
		       COALESCE(obj_description(p.oid, 'pg_publication'), '') AS comment
		FROM pg_publication p;
	`)
	if err != nil {
		return nil, fmt.Errorf("inspecting publications: %w", err)
	}
	defer func() { _ = pubRows.Close() }()
	pubNames := []string{}
	err = scanRows(pubRows, func(scan func(...any) error) error {
		var name, comment string
		var allTables, ins, upd, del, trunc bool
		if err := scan(&name, &allTables, &ins, &upd, &del, &trunc, &comment); err != nil {
			return err
		}
		live.Publications[strings.ToLower(name)] = &diff.PublicationState{
			Name:            name,
			Managed:         comment == schema.PublicationManagedComment,
			AllTables:       allTables,
			PublishInsert:   ins,
			PublishUpdate:   upd,
			PublishDelete:   del,
			PublishTruncate: trunc,
		}
		pubNames = append(pubNames, name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning publications: %w", err)
	}

	// 2. Publication table membership: only meaningful for non-ALL-TABLES
	// publications (FOR ALL TABLES does not create pg_publication_rel rows).
	if len(pubNames) > 0 {
		tableRows, err := dbtx.QueryContext(ctx, `
			SELECT p.pubname,
			       n.nspname,
			       c.relname
			FROM pg_publication p
			JOIN pg_publication_rel pr ON pr.prpubid = p.oid
			JOIN pg_class c ON c.oid = pr.prrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace;
		`)
		if err != nil {
			return nil, fmt.Errorf("inspecting publication tables: %w", err)
		}
		defer func() { _ = tableRows.Close() }()
		err = scanRows(tableRows, func(scan func(...any) error) error {
			var pub, schema, table string
			if err := scan(&pub, &schema, &table); err != nil {
				return err
			}
			if ps, ok := live.Publications[strings.ToLower(pub)]; ok {
				ps.Tables = append(ps.Tables, strings.ToLower(schema)+"."+strings.ToLower(table))
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scanning publication tables: %w", err)
		}

		// Schema-level memberships live in pg_publication_namespace
		// (PostgreSQL 15+). Probe the server version first: a failed query
		// aborts the surrounding transaction, so pre-15 servers must never
		// see the query at all.
		schemaPubsSupported, err := serverAtLeast15(ctx, dbtx)
		if err != nil {
			return nil, fmt.Errorf("inspecting publication schemas: %w", err)
		}
		if schemaPubsSupported {
			schemaRows, err := dbtx.QueryContext(ctx, `
				SELECT p.pubname, n.nspname
				FROM pg_publication p
				JOIN pg_publication_namespace pn ON pn.pnpubid = p.oid
				JOIN pg_namespace n ON n.oid = pn.pnnspid
				WHERE NOT p.puballtables;
			`)
			if err != nil {
				return nil, fmt.Errorf("inspecting publication schemas: %w", err)
			}
			defer func() { _ = schemaRows.Close() }()
			err = scanRows(schemaRows, func(scan func(...any) error) error {
				var pub, schema string
				if err := scan(&pub, &schema); err != nil {
					return err
				}
				if ps, ok := live.Publications[strings.ToLower(pub)]; ok {
					ps.Schemas = append(ps.Schemas, strings.ToLower(schema))
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("scanning publication schemas: %w", err)
			}
		}
	}

	for _, ps := range live.Publications {
		slices.Sort(ps.Tables)
		slices.Sort(ps.Schemas)
	}

	// 3. Event triggers with markers.
	etRows, err := dbtx.QueryContext(ctx, `
		SELECT e.evtname,
		       e.evtevent,
		       COALESCE(e.evttags, '{}') AS evtTags,
		       p.proname,
		       e.evtenabled,
		       COALESCE(obj_description(e.oid, 'pg_event_trigger'), '') AS comment
		FROM pg_event_trigger e
		JOIN pg_proc p ON p.oid = e.evtfoid;
	`)
	if err != nil {
		return nil, fmt.Errorf("inspecting event triggers: %w", err)
	}
	defer func() { _ = etRows.Close() }()
	err = scanRows(etRows, func(scan func(...any) error) error {
		var name, event, function, comment, enabled string
		var tags []string
		if err := scan(&name, &event, &tags, &function, &enabled, &comment); err != nil {
			return err
		}
		live.EventTriggers[strings.ToLower(name)] = &diff.EventTriggerState{
			Name:     name,
			Managed:  comment == schema.EventTriggerManagedComment,
			Event:    strings.ToUpper(event),
			Tags:     tags,
			Function: function,
			// evtenabled: 'O' = enabled (origin), anything else (D/K/A
			// disabled states) reports false.
			Enabled: enabled == "O",
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning event triggers: %w", err)
	}
	return live, nil
}

// CatalogState adapts the inspected live state to the dialect-independent
// diff.CatalogLiveState shape consumed by diff.CatalogDiff.
func (l *LiveCatalog) CatalogState() *diff.CatalogLiveState {
	state := &diff.CatalogLiveState{
		Publications:  make(map[string]*diff.PublicationState, len(l.Publications)),
		EventTriggers: make(map[string]*diff.EventTriggerState, len(l.EventTriggers)),
	}
	maps.Copy(state.Publications, l.Publications)
	maps.Copy(state.EventTriggers, l.EventTriggers)
	return state
}

// serverAtLeast15 reports whether the server understands
// pg_publication_namespace (PostgreSQL 15+).
func serverAtLeast15(ctx context.Context, dbtx dialect.DBTX) (bool, error) {
	var num int
	if err := dbtx.QueryRowContext(ctx, `SHOW server_version_num;`).Scan(&num); err != nil {
		return false, err
	}
	return num >= 150000, nil
}

// EventTriggerFunctionExists reports whether the named unqualified function
// exists (any signature). Used to refuse event-trigger creation when the
// trigger function is missing.
func EventTriggerFunctionExists(ctx context.Context, dbtx dialect.DBTX, functionName string) (bool, error) {
	var exists bool
	err := dbtx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_proc p WHERE p.proname = lower($1));
	`, functionName).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking event-trigger function %q: %w", functionName, err)
	}
	return exists, nil
}

// GenerateCreatePublicationSQL renders the desired publication.
func GenerateCreatePublicationSQL(p *schema.Publication) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE PUBLICATION %q", p.Name)
	switch {
	case p.AllTables:
		sb.WriteString(" FOR ALL TABLES")
	case len(p.Tables) > 0:
		sb.WriteString(" FOR TABLE ")
		sb.WriteString(strings.Join(quoteQualifiedList(p.Tables), ", "))
	case len(p.Schemas) > 0:
		sb.WriteString(" FOR TABLES IN SCHEMA ")
		sb.WriteString(strings.Join(quoteQualifiedList(p.Schemas), ", "))
	}
	fmt.Fprintf(&sb, " WITH (publish = '%s');", publishFlagString(p))
	return sb.String()
}

// GenerateAlterPublicationSQL renders the drift reconciliation statements
// (membership and publish-flag changes) for one publication.
func GenerateAlterPublicationSQL(want, old *schema.Publication) string {
	var out []string
	if want.AllTables != old.AllTables {
		if want.AllTables {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %q SET ALL TABLES;", want.Name))
		} else {
			// Leaving FOR ALL TABLES: set the explicit membership in one
			// statement (empty desired membership simply clears everything
			// via the DROP statements below — the SET line is skipped).
			switch {
			case len(want.Tables) > 0:
				out = append(out, fmt.Sprintf("ALTER PUBLICATION %q SET TABLE %s;", want.Name, strings.Join(quoteQualifiedList(want.Tables), ", ")))
			case len(want.Schemas) > 0:
				out = append(out, fmt.Sprintf("ALTER PUBLICATION %q SET TABLES IN SCHEMA %s;", want.Name, strings.Join(quoteQualifiedList(want.Schemas), ", ")))
			}
		}
	}
	if !want.AllTables {
		addTables, dropTables := nameSetDelta(canonicalPubTables(old.Tables), want.Tables)
		if len(dropTables) > 0 {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %q DROP TABLE %s;", want.Name, strings.Join(quoteQualifiedList(dropTables), ", ")))
		}
		if len(addTables) > 0 {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %q ADD TABLE %s;", want.Name, strings.Join(quoteQualifiedList(addTables), ", ")))
		}
		addSchemas, dropSchemas := nameSetDelta(old.Schemas, want.Schemas)
		if len(dropSchemas) > 0 {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %q DROP TABLES IN SCHEMA %s;", want.Name, strings.Join(quoteQualifiedList(dropSchemas), ", ")))
		}
		if len(addSchemas) > 0 {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %q ADD TABLES IN SCHEMA %s;", want.Name, strings.Join(quoteQualifiedList(addSchemas), ", ")))
		}
	}
	if want.PublishInsert != old.PublishInsert || want.PublishUpdate != old.PublishUpdate ||
		want.PublishDelete != old.PublishDelete || want.PublishTruncate != old.PublishTruncate {
		out = append(out, fmt.Sprintf("ALTER PUBLICATION %q SET (publish = '%s');", want.Name, publishFlagString(want)))
	}
	return strings.Join(out, "\n")
}

// GenerateDropPublicationSQL renders DROP PUBLICATION.
func GenerateDropPublicationSQL(name string) string {
	return fmt.Sprintf("DROP PUBLICATION %q;", name)
}

// GeneratePublicationCommentSQL stamps the managed-publication marker.
func GeneratePublicationCommentSQL(name string) string {
	return fmt.Sprintf("COMMENT ON PUBLICATION %q IS '%s';", name, schema.PublicationManagedComment)
}

// GenerateCreateEventTriggerSQL renders the desired event trigger.
func GenerateCreateEventTriggerSQL(e *schema.EventTrigger) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE EVENT TRIGGER %q ON %s", e.Name, strings.ToUpper(e.Event))
	if len(e.Tags) > 0 {
		quoted := make([]string, 0, len(e.Tags))
		for _, tag := range e.Tags {
			quoted = append(quoted, fmt.Sprintf("'%s'", tag))
		}
		fmt.Fprintf(&sb, " WHEN TAG IN (%s)", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&sb, " EXECUTE FUNCTION %s();", e.Function)
	return sb.String()
}

// GenerateAlterEventTriggerEnabledSQL renders ENABLE/DISABLE for the
// desired enabled state.
func GenerateAlterEventTriggerEnabledSQL(e *schema.EventTrigger) string {
	if e.Enabled {
		return fmt.Sprintf("ALTER EVENT TRIGGER %q ENABLE;", e.Name)
	}
	return fmt.Sprintf("ALTER EVENT TRIGGER %q DISABLE;", e.Name)
}

// GenerateAlterEventTriggerSQL renders drift reconciliation for one event
// trigger. PostgreSQL has no ALTER EVENT TRIGGER for event/tags/function,
// so definition drift renders a DROP+CREATE replacement pair; enabled-only
// drift renders ALTER EVENT TRIGGER ENABLE/DISABLE.
func GenerateAlterEventTriggerSQL(want, old *schema.EventTrigger) string {
	definitionDrift := old == nil || !strings.EqualFold(want.Event, old.Event) ||
		!strings.EqualFold(want.Function, old.Function) ||
		!slices.Equal(canonicalTagList(want.Tags), canonicalTagList(old.Tags))
	if definitionDrift {
		return strings.Join([]string{
			GenerateDropEventTriggerSQL(want.Name),
			GenerateCreateEventTriggerSQL(want),
			GenerateEventTriggerCommentSQL(want.Name),
		}, "\n")
	}
	return GenerateAlterEventTriggerEnabledSQL(want)
}

// canonicalTagList lowercases and sorts a tag list for comparison.
func canonicalTagList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(t)))
	}
	slices.Sort(out)
	return out
}

// GenerateDropEventTriggerSQL renders DROP EVENT TRIGGER.
func GenerateDropEventTriggerSQL(name string) string {
	return fmt.Sprintf("DROP EVENT TRIGGER %q;", name)
}

// GenerateEventTriggerCommentSQL stamps the managed-event-trigger marker.
func GenerateEventTriggerCommentSQL(name string) string {
	return fmt.Sprintf("COMMENT ON EVENT TRIGGER %q IS '%s';", name, schema.EventTriggerManagedComment)
}

// publishFlagString renders the comma-separated publish option list for the
// desired publication.
func publishFlagString(p *schema.Publication) string {
	var ops []string
	if p.PublishInsert {
		ops = append(ops, "insert")
	}
	if p.PublishUpdate {
		ops = append(ops, "update")
	}
	if p.PublishDelete {
		ops = append(ops, "delete")
	}
	if p.PublishTruncate {
		ops = append(ops, "truncate")
	}
	return strings.Join(ops, ", ")
}

// quoteQualifiedList quotes each (possibly schema-qualified) identifier.
func quoteQualifiedList(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		parts := strings.SplitN(n, ".", 2)
		for i := range parts {
			parts[i] = fmt.Sprintf("%q", parts[i])
		}
		out = append(out, strings.Join(parts, "."))
	}
	return out
}

// canonicalPubTables lowercases a table name list for set comparison.
func canonicalPubTables(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(s))
	}
	return out
}

// nameSetDelta returns (add, drop) lists between live and desired name sets.
func nameSetDelta(live, want []string) (add []string, drop []string) {
	liveSet := make(map[string]bool, len(live))
	for _, s := range live {
		liveSet[s] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, s := range want {
		wantSet[s] = true
	}
	for _, s := range want {
		if !liveSet[s] {
			add = append(add, s)
		}
	}
	for _, s := range live {
		if !wantSet[s] {
			drop = append(drop, s)
		}
	}
	return add, drop
}
