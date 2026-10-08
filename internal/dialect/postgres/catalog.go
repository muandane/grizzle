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

// LiveCatalog captures the publication/event-trigger/subscription/slot state
// relevant to a CatalogSQL diff.
type LiveCatalog struct {
	// Publications maps canonical publication identity -> live state.
	Publications map[string]*diff.PublicationState
	// EventTriggers maps canonical event-trigger identity -> live state.
	EventTriggers map[string]*diff.EventTriggerState
	// Subscriptions maps canonical subscription identity -> live state.
	Subscriptions map[string]*diff.SubscriptionState
	// ReplicationSlots maps canonical logical slot identity -> live state.
	ReplicationSlots map[string]*diff.ReplicationSlotState
}

// InspectLiveCatalog reads publications (with table membership and publish
// flags), event triggers, subscriptions, and logical replication slots plus
// grizzle-managed marker comments where COMMENT ON is supported.
func InspectLiveCatalog(ctx context.Context, dbtx dialect.DBTX) (*LiveCatalog, error) {
	live := &LiveCatalog{
		Publications:     make(map[string]*diff.PublicationState),
		EventTriggers:    make(map[string]*diff.EventTriggerState),
		Subscriptions:    make(map[string]*diff.SubscriptionState),
		ReplicationSlots: make(map[string]*diff.ReplicationSlotState),
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
		live.Publications[schema.CanonicalIdentifierKey(name)] = &diff.PublicationState{
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
			var pub, schemaName, table string
			if err := scan(&pub, &schemaName, &table); err != nil {
				return err
			}
			if ps, ok := live.Publications[schema.CanonicalIdentifierKey(pub)]; ok {
				ps.Tables = append(ps.Tables, canonicalLiveIdentifier(schemaName)+"."+canonicalLiveIdentifier(table))
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
				var pub, schemaName string
				if err := scan(&pub, &schemaName); err != nil {
					return err
				}
				if ps, ok := live.Publications[schema.CanonicalIdentifierKey(pub)]; ok {
					ps.Schemas = append(ps.Schemas, canonicalLiveIdentifier(schemaName))
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
		       n.nspname,
		       p.proname,
		       pg_get_function_identity_arguments(p.oid),
		       e.evtenabled,
		       COALESCE(obj_description(e.oid, 'pg_event_trigger'), '') AS comment
		FROM pg_event_trigger e
		JOIN pg_proc p ON p.oid = e.evtfoid
		JOIN pg_namespace n ON n.oid = p.pronamespace;
	`)
	if err != nil {
		return nil, fmt.Errorf("inspecting event triggers: %w", err)
	}
	defer func() { _ = etRows.Close() }()
	err = scanRows(etRows, func(scan func(...any) error) error {
		var name, event, functionSchema, functionName, identityArgs, comment, enabled string
		var tags []string
		if err := scan(&name, &event, &tags, &functionSchema, &functionName, &identityArgs, &enabled, &comment); err != nil {
			return err
		}
		live.EventTriggers[schema.CanonicalIdentifierKey(name)] = &diff.EventTriggerState{
			Name:     name,
			Managed:  comment == schema.EventTriggerManagedComment,
			Event:    strings.ToUpper(event),
			Tags:     tags,
			Function: canonicalLiveFunctionIdentity(functionSchema, functionName, identityArgs),
			// evtenabled: O = origin, A = always, R = replica, D =
			// disabled. Replica/always are enabled states even though they
			// only fire for their corresponding replication mode.
			Enabled: eventTriggerEnabled(enabled),
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning event triggers: %w", err)
	}

	// 4. Subscriptions in the current database with markers.
	subRows, err := dbtx.QueryContext(ctx, `
		SELECT s.subname,
		       s.subconninfo,
		       COALESCE(s.subslotname, ''),
		       s.subpublications,
		       s.subenabled,
		       COALESCE(obj_description(s.oid, 'pg_subscription'), '') AS comment
		FROM pg_subscription s
		WHERE s.subdbid = (SELECT oid FROM pg_database WHERE datname = current_database());
	`)
	if err != nil {
		return nil, fmt.Errorf("inspecting subscriptions: %w", err)
	}
	defer func() { _ = subRows.Close() }()
	subscriptionSlotNames := make(map[string]bool)
	err = scanRows(subRows, func(scan func(...any) error) error {
		var name, conninfo, slotName, comment string
		var pubs []string
		var enabled bool
		if err := scan(&name, &conninfo, &slotName, &pubs, &enabled, &comment); err != nil {
			return err
		}
		sortedPubs := append([]string(nil), pubs...)
		slices.Sort(sortedPubs)
		live.Subscriptions[schema.CanonicalIdentifierKey(name)] = &diff.SubscriptionState{
			Name:         name,
			Managed:      comment == schema.SubscriptionManagedComment,
			ConnInfo:     conninfo,
			SlotName:     slotName,
			Publications: sortedPubs,
			Enabled:      enabled,
		}
		if slotName != "" {
			subscriptionSlotNames[schema.CanonicalIdentifierKey(slotName)] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning subscriptions: %w", err)
	}

	// 5. Standalone logical replication slots for the current database.
	// Replication slots do not support COMMENT ON.
	slotRows, err := dbtx.QueryContext(ctx, `
		SELECT slot_name,
		       COALESCE(plugin, ''),
		       temporary,
		       active_pid
		FROM pg_replication_slots
		WHERE database = current_database()
		  AND slot_type = 'logical';
	`)
	if err != nil {
		return nil, fmt.Errorf("inspecting replication slots: %w", err)
	}
	defer func() { _ = slotRows.Close() }()
	err = scanRows(slotRows, func(scan func(...any) error) error {
		var name, plugin string
		var temporary bool
		var activePID *int
		if err := scan(&name, &plugin, &temporary, &activePID); err != nil {
			return err
		}
		key := schema.CanonicalIdentifierKey(name)
		live.ReplicationSlots[key] = &diff.ReplicationSlotState{
			Name:                name,
			Plugin:              plugin,
			Temporary:           temporary,
			ActivePID:           activePID,
			OwnedBySubscription: subscriptionSlotNames[key],
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning replication slots: %w", err)
	}
	return live, nil
}

func eventTriggerEnabled(status string) bool {
	switch strings.ToUpper(status) {
	case "O", "A", "R":
		return true
	default:
		return false
	}
}

func canonicalLiveFunctionIdentity(functionSchema, functionName, identityArgs string) string {
	name := canonicalLiveIdentifier(functionSchema) + "." + canonicalLiveIdentifier(functionName)
	return name + "(" + strings.TrimSpace(identityArgs) + ")"
}

// CatalogState adapts the inspected live state to the dialect-independent
// diff.CatalogLiveState shape consumed by diff.CatalogDiff.
func (l *LiveCatalog) CatalogState() *diff.CatalogLiveState {
	state := &diff.CatalogLiveState{
		Publications:     make(map[string]*diff.PublicationState, len(l.Publications)),
		EventTriggers:    make(map[string]*diff.EventTriggerState, len(l.EventTriggers)),
		Subscriptions:    make(map[string]*diff.SubscriptionState, len(l.Subscriptions)),
		ReplicationSlots: make(map[string]*diff.ReplicationSlotState, len(l.ReplicationSlots)),
	}
	maps.Copy(state.Publications, l.Publications)
	maps.Copy(state.EventTriggers, l.EventTriggers)
	maps.Copy(state.Subscriptions, l.Subscriptions)
	maps.Copy(state.ReplicationSlots, l.ReplicationSlots)
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

// EventTriggerFunctionExists reports whether the exact desired no-argument
// event-trigger function exists. PostgreSQL event triggers require a regular
// function returning event_trigger; procedures, aggregates, and similarly
// named routines are not compatible.
func EventTriggerFunctionExists(ctx context.Context, dbtx dialect.DBTX, functionName string, lookupSchemas ...string) (bool, error) {
	return EventTriggerFunctionExistsWithShadowMap(ctx, dbtx, functionName, nil, lookupSchemas...)
}

// EventTriggerFunctionExistsWithShadowMap is the multi-schema form of
// EventTriggerFunctionExists. A qualified target-schema routine is resolved
// in its corresponding shadow schema, matching RewriteShadowSQL and the
// schema diff's target/shadow mapping.
func EventTriggerFunctionExistsWithShadowMap(ctx context.Context, dbtx dialect.DBTX, functionName string, shadowMap map[string]string, lookupSchemas ...string) (bool, error) {
	originalParts := splitQualifiedIdentifier(strings.TrimSpace(functionName))
	originalSchema := ""
	if len(originalParts) == 2 {
		originalSchema = decodeCatalogIdentifier(originalParts[0])
	}
	if len(shadowMap) > 0 {
		functionName = mapQualifiedFunctionSchema(functionName, shadowMap)
	}
	parts := splitQualifiedIdentifier(strings.TrimSpace(functionName))
	if len(parts) == 0 || len(parts) > 2 {
		return false, fmt.Errorf("checking event-trigger function %q: invalid qualified function name", functionName)
	}
	for i, part := range parts {
		parts[i] = decodeCatalogIdentifier(part)
		if parts[i] == "" {
			return false, fmt.Errorf("checking event-trigger function %q: empty function identifier", functionName)
		}
	}

	query := `
		SELECT EXISTS (
			SELECT 1
			FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE p.proname = $1
			  AND p.pronargs = 0
			  AND p.prokind = 'f'
			  AND p.prorettype = 'event_trigger'::regtype
`
	args := []any{parts[len(parts)-1]}
	if len(parts) == 2 {
		candidateSchemas := []string{parts[0]}
		if originalSchema != "" && originalSchema != parts[0] {
			candidateSchemas = append(candidateSchemas, originalSchema)
		}
		schemaPlaceholders := make([]string, 0, len(candidateSchemas))
		orderCases := make([]string, 0, len(candidateSchemas))
		for _, schemaName := range candidateSchemas {
			schemaPlaceholders = append(schemaPlaceholders, fmt.Sprintf("$%d", len(args)+1))
			args = append(args, schemaName)
			orderCases = append(orderCases, fmt.Sprintf("WHEN $%d THEN %d", len(args), len(orderCases)+1))
		}
		query += "\n\t\t\t  AND n.nspname IN (" + strings.Join(schemaPlaceholders, ", ") + ")" +
			"\n\t\t\tORDER BY CASE n.nspname " + strings.Join(orderCases, " ") + " END\n\t\t\tLIMIT 1"
	} else if len(lookupSchemas) > 0 {
		schemaPlaceholders := make([]string, 0, len(lookupSchemas))
		orderCases := make([]string, 0, len(lookupSchemas))
		for _, schemaName := range lookupSchemas {
			if strings.TrimSpace(schemaName) == "" {
				continue
			}
			schemaPlaceholders = append(schemaPlaceholders, fmt.Sprintf("$%d", len(args)+1))
			args = append(args, schemaName)
			orderCases = append(orderCases, fmt.Sprintf("WHEN $%d THEN %d", len(args), len(orderCases)+1))
		}
		if len(schemaPlaceholders) > 0 {
			query += "\n\t\t\t  AND n.nspname IN (" + strings.Join(schemaPlaceholders, ", ") + ")" +
				"\n\t\t\tORDER BY CASE n.nspname " + strings.Join(orderCases, " ") + " END\n\t\t\tLIMIT 1"
		} else {
			query += "\n\t\t\tLIMIT 1"
		}
	} else {
		query += `
			  AND n.nspname = ANY (current_schemas(true))
			ORDER BY array_position(current_schemas(true), n.nspname)
			LIMIT 1`
	}
	query += `
		);`
	var exists bool
	err := dbtx.QueryRowContext(ctx, query, args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking event-trigger function %q: %w", functionName, err)
	}
	return exists, nil
}

func mapQualifiedFunctionSchema(functionName string, shadowMap map[string]string) string {
	parts := splitQualifiedIdentifier(strings.TrimSpace(functionName))
	if len(parts) != 2 {
		return functionName
	}
	targetSchema := decodeCatalogIdentifier(parts[0])
	shadowSchema, ok := shadowSchemaForTarget(shadowMap, targetSchema)
	if !ok {
		return functionName
	}
	return quoteIdentifier(shadowSchema) + "." + parts[1]
}

func shadowSchemaForTarget(shadowMap map[string]string, targetSchema string) (string, bool) {
	for configuredSchema, shadowSchema := range shadowMap {
		if configuredSchema == targetSchema || decodeCatalogIdentifier(configuredSchema) == targetSchema {
			return shadowSchema, true
		}
	}
	return "", false
}

// GenerateCreatePublicationSQL renders the desired publication.
func GenerateCreatePublicationSQL(p *schema.Publication) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE PUBLICATION %s", quoteIdentifier(p.Name))
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
	if !p.AllTables && len(p.Tables) > 0 && len(p.Schemas) > 0 {
		fmt.Fprintf(&sb, "\nALTER PUBLICATION %s ADD TABLES IN SCHEMA %s;",
			quoteIdentifier(p.Name),
			strings.Join(quoteQualifiedList(p.Schemas), ", "))
	}
	return sb.String()
}

// GenerateAlterPublicationSQL renders the drift reconciliation statements
// (membership and publish-flag changes) for one publication.
func GenerateAlterPublicationSQL(want, old *schema.Publication) string {
	var out []string
	if want.AllTables != old.AllTables {
		if want.AllTables {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %s SET ALL TABLES;", quoteIdentifier(want.Name)))
		} else {
			// PostgreSQL has no valid empty SET TABLE form. Recreate the
			// publication through CatalogDiff, which emits a gated
			// DROP_PUBLICATION plus a replacement CREATE_PUBLICATION.
			if len(want.Tables) == 0 && len(want.Schemas) == 0 {
				return ""
			}
			// Leaving FOR ALL TABLES resets explicit membership. Set one
			// base membership form, then add the other membership kind if
			// both are desired.
			switch {
			case len(want.Tables) > 0:
				out = append(out, fmt.Sprintf("ALTER PUBLICATION %s SET TABLE %s;", quoteIdentifier(want.Name), strings.Join(quoteQualifiedList(want.Tables), ", ")))
				if len(want.Schemas) > 0 {
					out = append(out, fmt.Sprintf("ALTER PUBLICATION %s ADD TABLES IN SCHEMA %s;", quoteIdentifier(want.Name), strings.Join(quoteQualifiedList(want.Schemas), ", ")))
				}
			case len(want.Schemas) > 0:
				out = append(out, fmt.Sprintf("ALTER PUBLICATION %s SET TABLES IN SCHEMA %s;", quoteIdentifier(want.Name), strings.Join(quoteQualifiedList(want.Schemas), ", ")))
			}
		}
	}
	if !want.AllTables && !old.AllTables {
		addTables, dropTables := nameSetDelta(canonicalPubTables(old.Tables), canonicalPubTables(want.Tables))
		if len(dropTables) > 0 {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %s DROP TABLE %s;", quoteIdentifier(want.Name), strings.Join(quoteQualifiedList(dropTables), ", ")))
		}
		if len(addTables) > 0 {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %s ADD TABLE %s;", quoteIdentifier(want.Name), strings.Join(quoteQualifiedList(addTables), ", ")))
		}
		addSchemas, dropSchemas := nameSetDelta(old.Schemas, want.Schemas)
		if len(dropSchemas) > 0 {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %s DROP TABLES IN SCHEMA %s;", quoteIdentifier(want.Name), strings.Join(quoteQualifiedList(dropSchemas), ", ")))
		}
		if len(addSchemas) > 0 {
			out = append(out, fmt.Sprintf("ALTER PUBLICATION %s ADD TABLES IN SCHEMA %s;", quoteIdentifier(want.Name), strings.Join(quoteQualifiedList(addSchemas), ", ")))
		}
	}
	if want.PublishInsert != old.PublishInsert || want.PublishUpdate != old.PublishUpdate ||
		want.PublishDelete != old.PublishDelete || want.PublishTruncate != old.PublishTruncate {
		out = append(out, fmt.Sprintf("ALTER PUBLICATION %s SET (publish = '%s');", quoteIdentifier(want.Name), publishFlagString(want)))
	}
	return strings.Join(out, "\n")
}

// GenerateDropPublicationSQL renders DROP PUBLICATION.
func GenerateDropPublicationSQL(name string) string {
	return fmt.Sprintf("DROP PUBLICATION %s;", quoteIdentifier(name))
}

// GeneratePublicationCommentSQL stamps the managed-publication marker.
func GeneratePublicationCommentSQL(name string) string {
	return fmt.Sprintf("COMMENT ON PUBLICATION %s IS '%s';", quoteIdentifier(name), schema.PublicationManagedComment)
}

// GenerateCreateEventTriggerSQL renders the desired event trigger.
func GenerateCreateEventTriggerSQL(e *schema.EventTrigger) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE EVENT TRIGGER %s ON %s", quoteIdentifier(e.Name), strings.ToUpper(e.Event))
	if len(e.Tags) > 0 {
		quoted := make([]string, 0, len(e.Tags))
		for _, tag := range e.Tags {
			quoted = append(quoted, "'"+strings.ReplaceAll(tag, "'", "''")+"'")
		}
		fmt.Fprintf(&sb, " WHEN TAG IN (%s)", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&sb, " EXECUTE FUNCTION %s();", quoteQualifiedIdentifierPreservingCase(e.Function))
	if !e.Enabled {
		sb.WriteString("\n")
		sb.WriteString(GenerateAlterEventTriggerEnabledSQL(e))
	}
	return sb.String()
}

// GenerateAlterEventTriggerEnabledSQL renders ENABLE/DISABLE for the
// desired enabled state.
func GenerateAlterEventTriggerEnabledSQL(e *schema.EventTrigger) string {
	if e.Enabled {
		return fmt.Sprintf("ALTER EVENT TRIGGER %s ENABLE;", quoteIdentifier(e.Name))
	}
	return fmt.Sprintf("ALTER EVENT TRIGGER %s DISABLE;", quoteIdentifier(e.Name))
}

// GenerateAlterEventTriggerSQL renders drift reconciliation for one event
// trigger. PostgreSQL has no ALTER EVENT TRIGGER for event/tags/function,
// so definition drift renders a DROP+CREATE replacement pair; enabled-only
// drift renders ALTER EVENT TRIGGER ENABLE/DISABLE.
func GenerateAlterEventTriggerSQL(want, old *schema.EventTrigger) string {
	definitionDrift := old == nil || !strings.EqualFold(want.Event, old.Event) ||
		want.Function != old.Function ||
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
	return fmt.Sprintf("DROP EVENT TRIGGER %s;", quoteIdentifier(name))
}

// GenerateEventTriggerCommentSQL stamps the managed-event-trigger marker.
func GenerateEventTriggerCommentSQL(name string) string {
	return fmt.Sprintf("COMMENT ON EVENT TRIGGER %s IS '%s';", quoteIdentifier(name), schema.EventTriggerManagedComment)
}

// GenerateCreateSubscriptionSQL renders CREATE SUBSCRIPTION with a redacted
// connection string suitable for Step.SQL / plan documents.
func GenerateCreateSubscriptionSQL(s *schema.Subscription) string {
	return generateCreateSubscriptionSQL(s, true)
}

// GenerateCreateSubscriptionApplySQL renders CREATE SUBSCRIPTION with the
// plaintext connection string from IR for apply.
func GenerateCreateSubscriptionApplySQL(s *schema.Subscription) string {
	return generateCreateSubscriptionSQL(s, false)
}

func generateCreateSubscriptionSQL(s *schema.Subscription, redact bool) string {
	conn := s.ConnInfo
	if redact {
		conn = schema.PasswordRedacted
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE SUBSCRIPTION %s CONNECTION '%s' PUBLICATION %s",
		quoteIdentifier(s.Name),
		escapeSQLString(conn),
		strings.Join(quoteQualifiedList(s.Publications), ", "))
	var withOpts []string
	if !s.Enabled {
		withOpts = append(withOpts, "enabled = false")
	}
	if !s.CopyData {
		withOpts = append(withOpts, "copy_data = false")
	}
	if s.SlotName != "" && schema.CanonicalIdentifierKey(s.SlotName) != schema.CanonicalIdentifierKey(s.Name) {
		withOpts = append(withOpts, fmt.Sprintf("slot_name = %s", quoteIdentifier(s.SlotName)))
	}
	if len(withOpts) > 0 {
		fmt.Fprintf(&sb, " WITH (%s)", strings.Join(withOpts, ", "))
	}
	sb.WriteByte(';')
	return sb.String()
}

// GenerateAlterSubscriptionSQL renders CONNECTION / ENABLE / DISABLE /
// SET PUBLICATION drift for one subscription. Connection strings in the
// returned SQL are redacted.
func GenerateAlterSubscriptionSQL(want, old *schema.Subscription) string {
	return generateAlterSubscriptionSQL(want, old, true)
}

// GenerateAlterSubscriptionApplySQL renders alter SQL with plaintext conninfo.
func GenerateAlterSubscriptionApplySQL(want, old *schema.Subscription) string {
	return generateAlterSubscriptionSQL(want, old, false)
}

func generateAlterSubscriptionSQL(want, old *schema.Subscription, redact bool) string {
	var out []string
	if old == nil || want.ConnInfo != old.ConnInfo {
		conn := want.ConnInfo
		if redact {
			conn = schema.PasswordRedacted
		}
		out = append(out, fmt.Sprintf("ALTER SUBSCRIPTION %s CONNECTION '%s';",
			quoteIdentifier(want.Name), escapeSQLString(conn)))
	}
	if old == nil || want.Enabled != old.Enabled {
		if want.Enabled {
			out = append(out, fmt.Sprintf("ALTER SUBSCRIPTION %s ENABLE;", quoteIdentifier(want.Name)))
		} else {
			out = append(out, fmt.Sprintf("ALTER SUBSCRIPTION %s DISABLE;", quoteIdentifier(want.Name)))
		}
	}
	if old == nil || !slices.Equal(canonicalPubNameList(want.Publications), canonicalPubNameList(old.Publications)) {
		out = append(out, fmt.Sprintf("ALTER SUBSCRIPTION %s SET PUBLICATION %s;",
			quoteIdentifier(want.Name), strings.Join(quoteQualifiedList(want.Publications), ", ")))
	}
	return strings.Join(out, "\n")
}

func canonicalPubNameList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(p)))
	}
	slices.Sort(out)
	return out
}

// GenerateDropSubscriptionSQL renders a drop that keeps the remote replication
// slot: DISABLE, disassociate slot_name, then DROP. Per PostgreSQL docs,
// DROP SUBSCRIPTION alone drops the remote slot by default.
func GenerateDropSubscriptionSQL(name string) string {
	id := quoteIdentifier(name)
	return strings.Join([]string{
		fmt.Sprintf("ALTER SUBSCRIPTION %s DISABLE;", id),
		fmt.Sprintf("ALTER SUBSCRIPTION %s SET (slot_name = NONE);", id),
		fmt.Sprintf("DROP SUBSCRIPTION %s;", id),
	}, "\n")
}

// GenerateSubscriptionCommentSQL stamps the managed-subscription marker.
func GenerateSubscriptionCommentSQL(name string) string {
	return fmt.Sprintf("COMMENT ON SUBSCRIPTION %s IS '%s';", quoteIdentifier(name), schema.SubscriptionManagedComment)
}

// GenerateCreateReplicationSlotSQL renders pg_create_logical_replication_slot.
func GenerateCreateReplicationSlotSQL(s *schema.ReplicationSlot) string {
	if s.Temporary {
		return fmt.Sprintf("SELECT pg_create_logical_replication_slot('%s', '%s', true);",
			escapeSQLString(s.Name), escapeSQLString(s.Plugin))
	}
	return fmt.Sprintf("SELECT pg_create_logical_replication_slot('%s', '%s');",
		escapeSQLString(s.Name), escapeSQLString(s.Plugin))
}

// GenerateDropReplicationSlotSQL renders pg_drop_replication_slot.
func GenerateDropReplicationSlotSQL(name string) string {
	return fmt.Sprintf("SELECT pg_drop_replication_slot('%s');", escapeSQLString(name))
}

func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
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
		out = append(out, quoteQualifiedIdentifier(n))
	}
	return out
}

func splitQualifiedIdentifier(name string) []string {
	return schema.ParseQualifiedIdentifier(name)
}

func quoteIdentifierPart(part string) string {
	return quoteIdentifier(decodeCatalogIdentifier(part))
}

func quoteQualifiedIdentifier(identifier string) string {
	parts := splitQualifiedIdentifier(identifier)
	for i, part := range parts {
		parts[i] = quoteIdentifierPart(part)
	}
	return strings.Join(parts, ".")
}

func quoteQualifiedIdentifierPreservingCase(identifier string) string {
	parts := splitQualifiedIdentifier(identifier)
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
			parts[i] = quoteIdentifierPart(part)
		} else {
			parts[i] = quoteIdentifier(part)
		}
	}
	return strings.Join(parts, ".")
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func decodeCatalogIdentifier(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if len(identifier) >= 2 && identifier[0] == '"' && identifier[len(identifier)-1] == '"' {
		return strings.ReplaceAll(identifier[1:len(identifier)-1], `""`, `"`)
	}
	return strings.ToLower(identifier)
}

func canonicalLiveIdentifier(identifier string) string {
	if isSimpleLowerIdentifier(identifier) {
		return strings.ToLower(identifier)
	}
	return quoteIdentifier(identifier)
}

func isSimpleLowerIdentifier(identifier string) bool {
	if identifier == "" {
		return false
	}
	for i, r := range identifier {
		if i == 0 {
			if r != '_' && (r < 'a' || r > 'z') {
				return false
			}
			continue
		}
		if r != '_' && (r < 'a' || r > 'z') &&
			(r < '0' || r > '9') && r != '$' {
			return false
		}
	}
	return true
}

// canonicalPubTables canonicalizes schema-qualified table names for set
// comparison while retaining quoted identifiers whose case or punctuation
// is significant.
func canonicalPubTables(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		parts := splitQualifiedIdentifier(s)
		for i, part := range parts {
			part = strings.TrimSpace(part)
			if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
				part = strings.ReplaceAll(part[1:len(part)-1], `""`, `"`)
			}
			if isSimpleLowerIdentifier(part) {
				parts[i] = strings.ToLower(part)
			} else {
				parts[i] = quoteIdentifier(part)
			}
		}
		out = append(out, strings.Join(parts, "."))
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
