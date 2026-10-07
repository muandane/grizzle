package schema

import (
	"fmt"
	"regexp"
	"strings"
)

// Role represents a managed database role declared in SchemaSQL or RolesSQL.
// Managed roles are always NOLOGIN group roles: Grizzle never sets or rotates
// passwords, so interactive-login roles stay outside the contract.
type Role struct {
	Name string `json:"name"`
}

// Grant represents a desired privilege grant scanned from SchemaSQL or
// RolesSQL.
type Grant struct {
	Grantee     string   `json:"grantee"`     // role name or PUBLIC
	ObjectKind  string   `json:"object_kind"` // TABLE, SEQUENCE, DATABASE, SCHEMA, FUNCTION
	ObjectName  string   `json:"object_name"` // as scanned; schema-qualified where applicable
	Privileges  []string `json:"privileges"`  // SELECT, INSERT, ... or kind-specific ALL expansion
	GrantOption bool     `json:"grant_option,omitempty"`
}

// RolesSpec is the desired role/privilege state parsed from a unified schema
// or side-channel SQL file.
type RolesSpec struct {
	Roles  map[string]*Role `json:"roles"`
	Grants []*Grant         `json:"grants"`

	revokes []*Grant
}

var (
	// createRoleRe captures the name from a CREATE ROLE/USER declaration.
	createRoleRe          = regexp.MustCompile(`(?i)^CREATE\s+(?:ROLE|USER)\s+` + catalogIdentifierPattern)
	createRoleStatementRe = regexp.MustCompile(`(?is)^CREATE\s+(?:ROLE|USER)\s+` + catalogIdentifierPattern + `(?:\s+(?:WITH\s+)?NOLOGIN)?$`)
	rolePasswordRe        = regexp.MustCompile(`(?i)\s+(?:WITH\s+)?(?:ENCRYPTED\s+)?PASSWORD\b`)

	// grantRe matches GRANT privilege[, ...] ON [TABLE|SEQUENCE|DATABASE|SCHEMA|FUNCTION]
	// object[, ...] TO grantee[, ...] [WITH GRANT OPTION]. The object slot
	// for FUNCTION keeps its argument list verbatim. Captures: (1) privileges,
	// (2) optional object kind, (3) object list, (4) grantee list,
	// (5) optional "WITH GRANT OPTION".
	grantRe = regexp.MustCompile(`(?is)^GRANT\s+(.+?)\s+ON\s+(?:(TABLE|SEQUENCE|DATABASE|SCHEMA|FUNCTION)\s+)?(.+?)\s+TO\s+(.+?)(\s+WITH\s+GRANT\s+OPTION)?\s*$`)

	// revokeRe matches the desired-state counterpart of grantRe. Captures:
	// (1) optional "GRANT OPTION FOR", (2) privileges, (3) optional object
	// kind, (4) object list, (5) grantee list.
	revokeRe = regexp.MustCompile(`(?is)^REVOKE\s+(GRANT\s+OPTION\s+FOR\s+)?(.+?)\s+ON\s+(?:(TABLE|SEQUENCE|DATABASE|SCHEMA|FUNCTION)\s+)?(.+?)\s+FROM\s+(.+?)(?:\s+(?:CASCADE|RESTRICT))?\s*$`)
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
// file. Role-membership statements remain outside the managed IR. Multiple
// objects/grantees/privileges in one statement expand to one Grant per
// (object, grantee) pair with a deduplicated privilege list. ALL is expanded
// to the kind-specific full privilege set so live ACL introspection compares
// without keyword translation.
func ParseRolesSQL(sql string) *RolesSpec {
	spec := &RolesSpec{Roles: make(map[string]*Role)}
	var revokes []*Grant
	for _, stmt := range SplitStatements(sql) {
		trimmed := stripLeadingComments(stmt)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "CREATE ROLE") || strings.HasPrefix(upper, "CREATE USER"):
			if m := createRoleRe.FindStringSubmatch(trimmed); m != nil {
				name := statementIdentifier(m)
				if name != "" {
					spec.Roles[strings.ToLower(name)] = &Role{Name: name}
				}
			}
		case strings.HasPrefix(upper, "GRANT"):
			spec.Grants = append(spec.Grants, parseGrantStatement(trimmed)...)
		case strings.HasPrefix(upper, "REVOKE"):
			revokes = append(revokes, parseRevokeStatement(trimmed)...)
		}
	}
	applyRevokes(spec, revokes)
	spec.revokes = revokes
	return spec
}

// MergeRolesSpecs combines role desired state from SchemaSQL with the
// optional RolesSQL side-channel. Entries from the side-channel replace
// SchemaSQL entries with the same role or grant identity, making the explicit
// side-channel authoritative on duplicates.
func MergeRolesSpecs(schemaSpec, sideSpec *RolesSpec) *RolesSpec {
	out := &RolesSpec{Roles: make(map[string]*Role)}
	for _, spec := range []*RolesSpec{schemaSpec, sideSpec} {
		if spec == nil {
			continue
		}
		for key, role := range spec.Roles {
			out.Roles[key] = &Role{Name: role.Name}
		}
	}

	out.Grants = mergeGrantSpecs(schemaSpec, sideSpec)
	if sideSpec != nil {
		applyRevokes(out, sideSpec.revokes)
	}
	return out
}

func mergeGrantSpecs(schemaSpec, sideSpec *RolesSpec) []*Grant {
	var out []*Grant
	for _, spec := range []*RolesSpec{schemaSpec, sideSpec} {
		if spec == nil {
			continue
		}
		var next []*Grant
		for _, grant := range spec.Grants {
			replaced := false
			for i, existing := range next {
				if sameGrantTarget(existing, grant) {
					next[i] = mergeGrant(existing, grant)
					replaced = true
					break
				}
			}
			if !replaced {
				next = append(next, cloneGrant(grant))
			}
		}
		if spec == sideSpec {
			for _, grant := range next {
				replaced := false
				for i, existing := range out {
					if sameGrantTarget(existing, grant) {
						out[i] = grant
						replaced = true
						break
					}
				}
				if !replaced {
					out = append(out, grant)
				}
			}
			continue
		}
		out = append(out, next...)
	}
	return out
}

func mergeGrant(a, b *Grant) *Grant {
	out := cloneGrant(a)
	for _, privilege := range b.Privileges {
		if !containsFold(out.Privileges, privilege) {
			out.Privileges = append(out.Privileges, privilege)
		}
	}
	out.GrantOption = out.GrantOption || b.GrantOption
	return out
}

func cloneGrant(g *Grant) *Grant {
	if g == nil {
		return nil
	}
	return &Grant{
		Grantee:     g.Grantee,
		ObjectKind:  g.ObjectKind,
		ObjectName:  g.ObjectName,
		Privileges:  slicesCloneStrings(g.Privileges),
		GrantOption: g.GrantOption,
	}
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

	privileges := expandPrivileges(privList, kind)
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

// parseRevokeStatement expands one REVOKE statement into per-(object,
// grantee) records. A revoke is applied to the parsed desired grants after
// the full file is scanned, so statement order has declarative semantics.
func parseRevokeStatement(stmt string) []*Grant {
	m := revokeRe.FindStringSubmatch(stmt)
	if m == nil {
		return nil
	}
	privList := strings.TrimSpace(m[2])
	kind := strings.ToUpper(strings.TrimSpace(m[3]))
	if kind == "" {
		kind = "TABLE"
	}
	objectList := splitList(m[4])
	granteeList := splitList(m[5])
	grantOption := strings.TrimSpace(m[1]) != ""

	privileges := expandPrivileges(privList, kind)
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

func expandPrivileges(privList, kind string) []string {
	var privileges []string
	for _, p := range splitList(privList) {
		pu := strings.ToUpper(p)
		if pu == "ALL" || pu == "ALL PRIVILEGES" {
			privileges = append(privileges, kindPrivilegeAllMap[kind]...)
			continue
		}
		privileges = append(privileges, pu)
	}
	return dedupeStrings(privileges)
}

func applyRevokes(spec *RolesSpec, revokes []*Grant) {
	if len(revokes) == 0 || len(spec.Grants) == 0 {
		return
	}

	for _, revoke := range revokes {
		var kept []*Grant
		for _, grant := range spec.Grants {
			if !sameGrantTarget(grant, revoke) {
				kept = append(kept, grant)
				continue
			}
			if revoke.GrantOption {
				for _, privilege := range revoke.Privileges {
					if containsFold(grant.Privileges, privilege) {
						grant.GrantOption = false
						break
					}
				}
				kept = append(kept, grant)
				continue
			}
			remaining := grant.Privileges[:0]
			for _, privilege := range grant.Privileges {
				if !containsFold(revoke.Privileges, privilege) {
					remaining = append(remaining, privilege)
				}
			}
			grant.Privileges = remaining
			if len(grant.Privileges) > 0 {
				kept = append(kept, grant)
			}
		}
		spec.Grants = kept
	}
}

func sameGrantTarget(a, b *Grant) bool {
	return strings.EqualFold(a.ObjectKind, b.ObjectKind) &&
		strings.EqualFold(a.ObjectName, b.ObjectName) &&
		strings.EqualFold(a.Grantee, b.Grantee)
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// stripLeadingComments removes leading -- comment lines from a statement
// chunk so comment-only chunks reduce to the empty string.
func stripLeadingComments(s string) string {
	return stripLeadingSQLComments(s)
}

// splitList splits a comma-separated SQL identifier list, respecting quotes.
func splitList(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			cur.WriteByte(s[i])
			if inQuote && i+1 < len(s) && s[i+1] == '"' {
				cur.WriteByte(s[i+1])
				i++
				continue
			}
			inQuote = !inQuote
		case ',':
			if inQuote {
				cur.WriteByte(s[i])
				continue
			}
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(s[i])
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
// operator-created roles are never swept when they leave the desired state.
const RoleManagedComment = "grizzle-managed"

// ValidateRolesSQL enforces the RolesSQL statement contract. Supported role
// declarations and object-privilege GRANT/REVOKE statements are accepted;
// unsupported role configuration fails loudly instead of being silently
// ignored by the statement scan.
func ValidateRolesSQL(sql string) error {
	for _, stmt := range SplitStatements(sql) {
		trimmed := stripLeadingComments(stmt)
		if trimmed == "" {
			continue
		}
		if containsSQLCommentOutsideQuotes(trimmed) {
			return fmt.Errorf("unsupported SQL comment in RolesSQL statement: %q", trimmed)
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "CREATE ROLE"), strings.HasPrefix(upper, "CREATE USER"),
			strings.HasPrefix(upper, "ALTER ROLE"), strings.HasPrefix(upper, "ALTER USER"),
			strings.HasPrefix(upper, "DROP ROLE"), strings.HasPrefix(upper, "DROP USER"):
			switch {
			case strings.HasPrefix(upper, "ALTER ROLE"), strings.HasPrefix(upper, "ALTER USER"):
				return fmt.Errorf("role configuration is not supported yet in PR1; ALTER ROLE/USER is reserved for PR2: %q", trimmed)
			case strings.HasPrefix(upper, "DROP ROLE"), strings.HasPrefix(upper, "DROP USER"):
				return fmt.Errorf("DROP ROLE/USER is not supported in the declarative role contract: %q", trimmed)
			case rolePasswordRe.MatchString(trimmed):
				return fmt.Errorf("passworded roles are not supported yet in PR1: %q", trimmed)
			case createRoleStatementRe.FindStringSubmatch(trimmed) == nil:
				return fmt.Errorf("unsupported role declaration in RolesSQL (only CREATE ROLE/USER [NOLOGIN] is supported): %q", trimmed)
			}
		case strings.HasPrefix(upper, "GRANT"):
			if grantRe.FindStringSubmatch(trimmed) == nil {
				return fmt.Errorf("unsupported GRANT form in RolesSQL (expected GRANT <privileges> ON [KIND] <object> TO <grantee>): %q", trimmed)
			}
		case strings.HasPrefix(upper, "REVOKE"):
			matches := revokeRe.FindStringSubmatch(trimmed)
			if matches == nil {
				return fmt.Errorf("unsupported REVOKE form in RolesSQL (expected REVOKE <privileges> ON [KIND] <object> FROM <grantee>): %q", trimmed)
			}
			if strings.TrimSpace(matches[1]) != "" {
				return fmt.Errorf("per-privilege grant-option revocation is not supported in PR1: %q", trimmed)
			}
		default:
			return fmt.Errorf("unsupported statement in RolesSQL (only CREATE ROLE/USER [NOLOGIN] and GRANT/REVOKE are managed): %q", trimmed)
		}
	}
	return nil
}

// FormatGrantKey is a stable display key for grant records.
func FormatGrantKey(g *Grant) string {
	return fmt.Sprintf("%s:%s:%s", g.ObjectKind, g.ObjectName, g.Grantee)
}
