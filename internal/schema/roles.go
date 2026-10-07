package schema

import (
	"fmt"
	"regexp"
	"strings"
)

// Role represents a managed database role declared in RolesSQL. Managed
// roles are always NOLOGIN group roles: Grizzle never sets or rotates
// passwords, so interactive-login roles stay outside the contract.
type Role struct {
	Name string `json:"name"`
}

// Grant represents a desired privilege grant scanned from RolesSQL.
type Grant struct {
	Grantee     string   `json:"grantee"`     // role name or PUBLIC
	ObjectKind  string   `json:"object_kind"` // TABLE, SEQUENCE, DATABASE, SCHEMA, FUNCTION
	ObjectName  string   `json:"object_name"` // as scanned; schema-qualified where applicable
	Privileges  []string `json:"privileges"`  // SELECT, INSERT, ... or kind-specific ALL expansion
	GrantOption bool     `json:"grant_option,omitempty"`
}

// RolesSpec is the desired role/privilege state parsed from a RolesSQL file.
type RolesSpec struct {
	Roles  map[string]*Role `json:"roles"`
	Grants []*Grant         `json:"grants"`
}

var (
	// createRoleRe matches CREATE ROLE name (options are ignored: managed
	// roles are forced to NOLOGIN at render time).
	createRoleRe = regexp.MustCompile(`(?i)^CREATE\s+(?:ROLE|USER)\s+("([^"]+)"|[\w]+)`)

	// grantRe matches GRANT privilege[, ...] ON [TABLE|SEQUENCE|DATABASE|SCHEMA|FUNCTION]
	// object[, ...] TO grantee[, ...] [WITH GRANT OPTION]. The object slot
	// for FUNCTION keeps its argument list verbatim. Captures: (1) privileges,
	// (2) optional object kind, (3) object list, (4) grantee list,
	// (5) optional "WITH GRANT OPTION".
	grantRe = regexp.MustCompile(`(?is)^GRANT\s+(.+?)\s+ON\s+(?:(TABLE|SEQUENCE|DATABASE|SCHEMA|FUNCTION)\s+)?(.+?)\s+TO\s+(.+?)(\s+WITH\s+GRANT\s+OPTION)?\s*$`)
)

// Privilege sets used to expand ALL per object kind. They mirror the
// PostgreSQL defaults so aclexplode output compares 1:1 with desired state.
var (
	tablePrivileges     = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
	sequencePrivileges  = []string{"USAGE", "SELECT", "UPDATE"}
	databasePrivileges  = []string{"CONNECT", "CREATE", "TEMPORARY"}
	schemaPrivileges    = []string{"USAGE", "CREATE"}
	functionPrivileges  = []string{"EXECUTE"}
	kindPrivilegeAllMap = map[string][]string{
		"TABLE":    tablePrivileges,
		"SEQUENCE": sequencePrivileges,
		"DATABASE": databasePrivileges,
		"SCHEMA":   schemaPrivileges,
		"FUNCTION": functionPrivileges,
	}
)

// GrantKey returns the map key identifying a grant: object kind, object
// name, and grantee. Privilege lists are compared separately.
func GrantKey(kind, object, grantee string) string {
	return strings.ToUpper(kind) + ":" + object + ":" + strings.ToUpper(grantee)
}

// ParseRolesSQL extracts managed roles and privilege grants from a RolesSQL
// file. Statements that are not CREATE ROLE or GRANT are ignored. Multiple
// objects/grantees/privileges in one statement expand to one Grant per
// (object, grantee) pair with a deduplicated privilege list. ALL is expanded
// to the kind-specific full privilege set so live ACL introspection compares
// without keyword translation.
func ParseRolesSQL(sql string) *RolesSpec {
	spec := &RolesSpec{Roles: make(map[string]*Role)}
	for _, stmt := range SplitStatements(sql) {
		trimmed := stripLeadingComments(stmt)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "CREATE ROLE") || strings.HasPrefix(upper, "CREATE USER"):
			if m := createRoleRe.FindStringSubmatch(trimmed); m != nil {
				name := strings.Trim(m[1], `"`)
				if name != "" {
					spec.Roles[strings.ToLower(name)] = &Role{Name: name}
				}
			}
		case strings.HasPrefix(upper, "GRANT"):
			spec.Grants = append(spec.Grants, parseGrantStatement(trimmed)...)
		}
	}
	return spec
}

// parseGrantStatement expands one GRANT statement into per-(object, grantee)
// Grant records.
func parseGrantStatement(stmt string) []*Grant {
	m := grantRe.FindStringSubmatch(stmt)
	if m == nil {
		return nil
	}
	privList := strings.TrimSpace(m[1])
	kind := strings.ToUpper(strings.TrimSpace(m[2]))
	if kind == "" {
		kind = "TABLE"
	}
	objectList := splitList(m[3])
	granteeList := splitList(m[4])
	grantOption := strings.TrimSpace(m[5]) != ""

	var privileges []string
	for _, p := range splitList(privList) {
		pu := strings.ToUpper(p)
		if pu == "ALL" || pu == "ALL PRIVILEGES" {
			privileges = append(privileges, kindPrivilegeAllMap[kind]...)
			continue
		}
		privileges = append(privileges, pu)
	}
	privileges = dedupeStrings(privileges)
	if len(privileges) == 0 || len(objectList) == 0 || len(granteeList) == 0 {
		return nil
	}

	var out []*Grant
	for _, object := range objectList {
		for _, grantee := range granteeList {
			grantee = strings.Trim(grantee, `"`)
			if grantee == "" {
				continue
			}
			out = append(out, &Grant{
				Grantee:     strings.ToUpper(grantee),
				ObjectKind:  kind,
				ObjectName:  object,
				Privileges:  slicesCloneStrings(privileges),
				GrantOption: grantOption,
			})
		}
	}
	return out
}

// stripLeadingComments removes leading -- comment lines from a statement
// chunk so comment-only chunks reduce to the empty string.
func stripLeadingComments(s string) string {
	for {
		t := strings.TrimSpace(s)
		if strings.HasPrefix(t, "--") {
			idx := strings.IndexByte(t, '\n')
			if idx < 0 {
				return ""
			}
			s = t[idx+1:]
			continue
		}
		return t
	}
}

// splitList splits a comma-separated SQL identifier list, respecting quotes.
func splitList(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if tail := strings.TrimSpace(cur.String()); tail != "" {
		parts = append(parts, tail)
	}
	return parts
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func slicesCloneStrings(in []string) []string {
	return append([]string(nil), in...)
}

// RoleManagedComment is the catalog comment marker Grizzle stamps on roles it
// created. Only roles carrying this marker are considered managed, so
// operator-created roles are never swept when they leave RolesSQL.
const RoleManagedComment = "grizzle-managed"

// ValidateRolesSQL enforces the RolesSQL statement contract: only CREATE
// ROLE/USER and GRANT statements are accepted. Anything else fails loudly
// instead of being silently ignored by the statement scan.
func ValidateRolesSQL(sql string) error {
	for _, stmt := range SplitStatements(sql) {
		trimmed := stripLeadingComments(stmt)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "CREATE ROLE"), strings.HasPrefix(upper, "CREATE USER"),
			strings.HasPrefix(upper, "GRANT"):
			if strings.HasPrefix(upper, "GRANT") && grantRe.FindStringSubmatch(trimmed) == nil {
				return fmt.Errorf("unsupported GRANT form in RolesSQL (expected GRANT <privileges> ON [KIND] <object> TO <grantee>): %q", trimmed)
			}
		default:
			return fmt.Errorf("unsupported statement in RolesSQL (only CREATE ROLE and GRANT are managed): %q", trimmed)
		}
	}
	return nil
}

// FormatGrantKey is a stable display key for grant records.
func FormatGrantKey(g *Grant) string {
	return fmt.Sprintf("%s:%s:%s", g.ObjectKind, g.ObjectName, g.Grantee)
}
