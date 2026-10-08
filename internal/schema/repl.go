package schema

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Subscription represents a managed logical-replication subscription declared
// in SchemaSQL or CatalogSQL. Subscriptions are database-catalog objects: they
// bypass shadow compilation and are diffed against live pg_subscription state.
//
// CopyData mirrors WITH (copy_data = ...); when the WITH clause omits it,
// PostgreSQL's default of true is adopted. Enabled likewise defaults to true
// when omitted. CopyData is create-time only and is not diffed against live
// state (pg_subscription does not persist it).
type Subscription struct {
	Name         string   `json:"name"`
	ConnInfo     string   `json:"conninfo,omitempty"`
	SlotName     string   `json:"slot_name,omitempty"`
	Publications []string `json:"publications"` // sorted
	Enabled      bool     `json:"enabled"`
	CopyData     bool     `json:"copy_data"`
}

// ReplicationSlot represents a standalone logical replication slot declared
// via SELECT pg_create_logical_replication_slot(...). Slots owned by a
// managed subscription (subslotname) are not modeled as separate objects.
// COMMENT ON is not supported for replication slots; live-only slots are
// never swept — drops require an explicit SELECT pg_drop_replication_slot
// and AllowDropReplicationSlot.
type ReplicationSlot struct {
	Name      string `json:"name"`
	Plugin    string `json:"plugin"`
	Temporary bool   `json:"temporary,omitempty"` // logical only; physical slots are refused
}

// SubscriptionManagedComment is the catalog comment marker stamped on
// subscriptions Grizzle created. Only marker-stamped subscriptions absent
// from the desired state are considered for dropping.
const SubscriptionManagedComment = "grizzle-managed"

var (
	// createSubscriptionRe matches:
	// CREATE SUBSCRIPTION name CONNECTION '...' PUBLICATION pub [, ...] [WITH (...)]
	createSubscriptionRe = regexp.MustCompile(`(?is)^CREATE\s+SUBSCRIPTION\s+` + catalogIdentifierPattern + `\s+CONNECTION\s+('(?:''|[^'])*')\s+PUBLICATION\s+(.+?)(?:\s+WITH\s*\((.*)\))?\s*$`)

	alterSubscriptionRe = regexp.MustCompile(`(?is)^ALTER\s+SUBSCRIPTION\s+` + catalogIdentifierPattern + `\s+(.+)$`)
	dropSubscriptionRe  = regexp.MustCompile(`(?is)^DROP\s+SUBSCRIPTION\s+(?:IF\s+EXISTS\s+)?` + catalogIdentifierPattern + `(?:\s+(?:CASCADE|RESTRICT))?$`)

	alterSubscriptionConnectionRe = regexp.MustCompile(`(?is)^CONNECTION\s+('(?:''|[^'])*')$`)
	alterSubscriptionSetPubRe     = regexp.MustCompile(`(?is)^SET\s+PUBLICATION\s+(.+)$`)

	createLogicalSlotRe  = regexp.MustCompile(`(?is)^SELECT\s+pg_create_logical_replication_slot\s*\(\s*'((?:''|[^'])*)'\s*,\s*'((?:''|[^'])*)'(?:\s*,\s*(true|false))?\s*\)\s*$`)
	dropLogicalSlotRe    = regexp.MustCompile(`(?is)^SELECT\s+pg_drop_replication_slot\s*\(\s*'((?:''|[^'])*)'\s*\)\s*$`)
	createPhysicalSlotRe = regexp.MustCompile(`(?is)^SELECT\s+pg_create_physical_replication_slot\b`)
)

var supportedSubscriptionWithOptions = map[string]bool{
	"enabled":     true,
	"copy_data":   true,
	"slot_name":   true,
	"create_slot": true,
}

// parseSubscriptionStatement expands one CREATE SUBSCRIPTION statement.
func parseSubscriptionStatement(stmt string) *Subscription {
	m := createSubscriptionRe.FindStringSubmatch(stmt)
	if m == nil {
		return nil
	}
	name := statementIdentifier(m)
	if name == "" {
		return nil
	}
	connInfo, err := unquoteSQLString(m[3])
	if err != nil {
		return nil
	}
	pubs, ok := splitIdentifierList(m[4])
	if !ok || len(pubs) == 0 {
		return nil
	}
	decodedPubs := make([]string, 0, len(pubs))
	for _, p := range pubs {
		decodedPubs = append(decodedPubs, decodeIdentifier(p))
	}
	slices.Sort(decodedPubs)
	sub := &Subscription{
		Name:         name,
		ConnInfo:     connInfo,
		Publications: decodedPubs,
		Enabled:      true,
		CopyData:     true,
		SlotName:     name, // PostgreSQL default: slot name equals subscription name
	}
	if withClause := strings.TrimSpace(m[5]); withClause != "" {
		if !applySubscriptionWithClause(sub, withClause) {
			return nil
		}
	}
	return sub
}

func applySubscriptionWithClause(sub *Subscription, clause string) bool {
	opts, ok := parseSubscriptionWithOptions(clause)
	if !ok {
		return false
	}
	for key, value := range opts {
		switch key {
		case "enabled":
			b, ok := parseSQLBool(value)
			if !ok {
				return false
			}
			sub.Enabled = b
		case "copy_data":
			b, ok := parseSQLBool(value)
			if !ok {
				return false
			}
			sub.CopyData = b
		case "slot_name":
			name, err := decodeSubscriptionSlotName(value)
			if err != nil {
				return false
			}
			sub.SlotName = name
		case "create_slot":
			// Accepted for CREATE rendering parity; not stored on IR because
			// it is create-time only and has no live catalog counterpart.
			if _, ok := parseSQLBool(value); !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func parseSubscriptionWithOptions(clause string) (map[string]string, bool) {
	out := make(map[string]string)
	parts, ok := splitCommaOutsideQuotes(clause)
	if !ok {
		return nil, false
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			return nil, false
		}
		key := strings.ToLower(strings.TrimSpace(part[:eq]))
		value := strings.TrimSpace(part[eq+1:])
		if key == "" || value == "" {
			return nil, false
		}
		if !supportedSubscriptionWithOptions[key] {
			return nil, false
		}
		if _, exists := out[key]; exists {
			return nil, false
		}
		out[key] = value
	}
	return out, true
}

func decodeSubscriptionSlotName(value string) (string, error) {
	upper := strings.ToUpper(strings.TrimSpace(value))
	if upper == "NONE" {
		return "", fmt.Errorf("slot_name = NONE is not managed as a durable subscription slot")
	}
	if len(value) >= 2 && value[0] == '\'' {
		return unquoteSQLString(value)
	}
	return decodeIdentifier(value), nil
}

func parseSQLBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "on", "1", "yes":
		return true, true
	case "false", "off", "0", "no":
		return false, true
	default:
		return false, false
	}
}

func unquoteSQLString(lit string) (string, error) {
	lit = strings.TrimSpace(lit)
	if len(lit) < 2 || lit[0] != '\'' || lit[len(lit)-1] != '\'' {
		return "", fmt.Errorf("expected quoted string")
	}
	return strings.ReplaceAll(lit[1:len(lit)-1], "''", "'"), nil
}

func splitCommaOutsideQuotes(s string) ([]string, bool) {
	var parts []string
	start := 0
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if inDouble {
				continue
			}
			if inSingle && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			inSingle = !inSingle
		case '"':
			if inSingle {
				continue
			}
			if inDouble && i+1 < len(s) && s[i+1] == '"' {
				i++
				continue
			}
			inDouble = !inDouble
		case ',':
			if inSingle || inDouble {
				continue
			}
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if inSingle || inDouble {
		return nil, false
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts, true
}

func applySubscriptionAlterClause(sub *Subscription, clause string) bool {
	clause = strings.TrimSpace(clause)
	switch strings.ToUpper(clause) {
	case "ENABLE":
		sub.Enabled = true
		return true
	case "DISABLE":
		sub.Enabled = false
		return true
	}
	if matches := alterSubscriptionConnectionRe.FindStringSubmatch(clause); matches != nil {
		conn, err := unquoteSQLString(matches[1])
		if err != nil {
			return false
		}
		sub.ConnInfo = conn
		return true
	}
	if matches := alterSubscriptionSetPubRe.FindStringSubmatch(clause); matches != nil {
		pubs, ok := splitIdentifierList(matches[1])
		if !ok || len(pubs) == 0 {
			return false
		}
		decoded := make([]string, 0, len(pubs))
		for _, p := range pubs {
			decoded = append(decoded, decodeIdentifier(p))
		}
		slices.Sort(decoded)
		sub.Publications = decoded
		return true
	}
	return false
}

func parseReplicationSlotCreate(stmt string) *ReplicationSlot {
	m := createLogicalSlotRe.FindStringSubmatch(stmt)
	if m == nil {
		return nil
	}
	name := strings.ReplaceAll(m[1], "''", "'")
	plugin := strings.ReplaceAll(m[2], "''", "'")
	if name == "" || plugin == "" {
		return nil
	}
	slot := &ReplicationSlot{Name: name, Plugin: plugin}
	if m[3] != "" {
		slot.Temporary = strings.EqualFold(m[3], "true")
	}
	return slot
}

func parseReplicationSlotDropName(stmt string) string {
	m := dropLogicalSlotRe.FindStringSubmatch(stmt)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "''", "'")
}

func isSupportedSubscriptionAlter(clause string) bool {
	clause = strings.TrimSpace(clause)
	if containsSQLCommentOutsideQuotes(clause) {
		return false
	}
	upper := strings.ToUpper(clause)
	if upper == "ENABLE" || upper == "DISABLE" {
		return true
	}
	if matches := alterSubscriptionConnectionRe.FindStringSubmatch(clause); matches != nil {
		_, err := unquoteSQLString(matches[1])
		return err == nil
	}
	if matches := alterSubscriptionSetPubRe.FindStringSubmatch(clause); matches != nil {
		return nonEmptyIdentifierList(matches[1], false)
	}
	return false
}

func validateSubscriptionCreate(matches []string) error {
	if len(matches) < 5 {
		return fmt.Errorf("incomplete CREATE SUBSCRIPTION")
	}
	if _, err := unquoteSQLString(matches[3]); err != nil {
		return err
	}
	pubs, ok := splitIdentifierList(matches[4])
	if !ok || len(pubs) == 0 {
		return fmt.Errorf("PUBLICATION list required")
	}
	for _, p := range pubs {
		if !validCatalogIdentifierPart(p) {
			return fmt.Errorf("invalid publication name %q", p)
		}
	}
	if withClause := strings.TrimSpace(matches[5]); withClause != "" {
		if containsSQLCommentOutsideQuotes(withClause) {
			return fmt.Errorf("comments in WITH clause are not supported")
		}
		opts, ok := parseSubscriptionWithOptions(withClause)
		if !ok {
			return fmt.Errorf("unsupported or invalid WITH option in CREATE SUBSCRIPTION (supported: enabled, copy_data, slot_name, create_slot)")
		}
		for key, value := range opts {
			switch key {
			case "enabled", "copy_data", "create_slot":
				if _, ok := parseSQLBool(value); !ok {
					return fmt.Errorf("invalid boolean for %s", key)
				}
			case "slot_name":
				if _, err := decodeSubscriptionSlotName(value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// RedactSubscriptionConnInfoSQL replaces CONNECTION '...' literals with the
// redacted placeholder so plan documents, Plan.Hash(), and step SQL never echo
// subscription connection strings. Hash and Document must use the same
// redaction so ParsePlanJSON round-trips.
func RedactSubscriptionConnInfoSQL(sql string) string {
	if sql == "" || !strings.Contains(strings.ToUpper(sql), "CONNECTION") {
		return sql
	}
	var b strings.Builder
	upper := strings.ToUpper(sql)
	i := 0
	for i < len(sql) {
		idx := strings.Index(upper[i:], "CONNECTION")
		if idx < 0 {
			b.WriteString(sql[i:])
			break
		}
		idx += i
		if idx > 0 {
			prev := sql[idx-1]
			if (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') || (prev >= '0' && prev <= '9') || prev == '_' {
				b.WriteString(sql[i : idx+10])
				i = idx + 10
				continue
			}
		}
		b.WriteString(sql[i:idx])
		b.WriteString(sql[idx : idx+10]) // CONNECTION
		j := idx + 10
		for j < len(sql) && unicode.IsSpace(rune(sql[j])) {
			b.WriteByte(sql[j])
			j++
		}
		if j >= len(sql) || sql[j] != '\'' {
			i = j
			continue
		}
		end, err := scanSingleQuotedLiteral(sql, j)
		if err != nil {
			b.WriteString(sql[j:])
			break
		}
		b.WriteByte('\'')
		b.WriteString(PasswordRedacted)
		b.WriteByte('\'')
		i = end
	}
	return b.String()
}

// RedactSecretsSQL applies role-password and subscription-conninfo redaction.
func RedactSecretsSQL(sql string) string {
	return RedactSubscriptionConnInfoSQL(RedactRolePasswordsSQL(sql))
}

// SubscriptionHasUsableConnInfo reports whether subscription IR carries a real
// connection string suitable for apply (not empty and not the redaction
// placeholder).
func SubscriptionHasUsableConnInfo(sub *Subscription) bool {
	if sub == nil {
		return false
	}
	if sub.ConnInfo == "" || sub.ConnInfo == PasswordRedacted {
		return false
	}
	return true
}

func cloneSubscription(s *Subscription) *Subscription {
	if s == nil {
		return nil
	}
	out := *s
	out.Publications = append([]string(nil), s.Publications...)
	return &out
}

func cloneReplicationSlot(s *ReplicationSlot) *ReplicationSlot {
	if s == nil {
		return nil
	}
	out := *s
	return &out
}

func upsertSubscription(spec *CatalogSpec, sub *Subscription) {
	key := CanonicalIdentifierKey(sub.Name)
	for i, existing := range spec.Subscriptions {
		if CanonicalIdentifierKey(existing.Name) == key {
			spec.Subscriptions[i] = sub
			return
		}
	}
	spec.Subscriptions = append(spec.Subscriptions, sub)
}

func removeSubscription(spec *CatalogSpec, name string) {
	filtered := spec.Subscriptions[:0]
	for _, sub := range spec.Subscriptions {
		if CanonicalIdentifierKey(sub.Name) != CanonicalIdentifierKey(name) {
			filtered = append(filtered, sub)
		}
	}
	spec.Subscriptions = filtered
}

func findSubscription(spec *CatalogSpec, name string) *Subscription {
	for _, sub := range spec.Subscriptions {
		if CanonicalIdentifierKey(sub.Name) == CanonicalIdentifierKey(name) {
			return sub
		}
	}
	return nil
}

func upsertReplicationSlot(spec *CatalogSpec, slot *ReplicationSlot) {
	key := CanonicalIdentifierKey(slot.Name)
	for i, existing := range spec.ReplicationSlots {
		if CanonicalIdentifierKey(existing.Name) == key {
			spec.ReplicationSlots[i] = slot
			return
		}
	}
	spec.ReplicationSlots = append(spec.ReplicationSlots, slot)
}

func removeReplicationSlot(spec *CatalogSpec, name string) {
	filtered := spec.ReplicationSlots[:0]
	for _, slot := range spec.ReplicationSlots {
		if CanonicalIdentifierKey(slot.Name) != CanonicalIdentifierKey(name) {
			filtered = append(filtered, slot)
		}
	}
	spec.ReplicationSlots = filtered
}

func findReplicationSlot(spec *CatalogSpec, name string) *ReplicationSlot {
	for _, slot := range spec.ReplicationSlots {
		if CanonicalIdentifierKey(slot.Name) == CanonicalIdentifierKey(name) {
			return slot
		}
	}
	return nil
}

// SubscriptionExplicitlyDropped reports whether the desired catalog input
// contains a DROP SUBSCRIPTION for name.
func (s *CatalogSpec) SubscriptionExplicitlyDropped(name string) bool {
	return s != nil && s.droppedSubscriptions[CanonicalIdentifierKey(name)]
}

// ReplicationSlotExplicitlyDropped reports whether the desired catalog input
// contains SELECT pg_drop_replication_slot for name.
func (s *CatalogSpec) ReplicationSlotExplicitlyDropped(name string) bool {
	return s != nil && s.droppedReplicationSlots[CanonicalIdentifierKey(name)]
}
