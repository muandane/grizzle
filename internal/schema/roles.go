package schema

import (
	"fmt"
	"regexp"
	"strings"
)

// Role represents a managed database role declared in SchemaSQL or RolesSQL.
// Attributes beyond Name are optional. When LOGIN/NOLOGIN is omitted, CREATE
// ROLE keeps PostgreSQL's default (NOLOGIN). Password plaintext is never
// serialized (json:"-"); HasPassword records that a password is desired for
// plan hashing and redaction without embedding the secret.
type Role struct {
	Name            string `json:"name"`
	Login           *bool  `json:"login,omitempty"`
	Password        string `json:"-"`
	HasPassword     bool   `json:"has_password,omitempty"`
	ValidUntil      string `json:"valid_until,omitempty"`
	ConnectionLimit *int   `json:"connection_limit,omitempty"`
	Inherit         *bool  `json:"inherit,omitempty"`
	CreateDB        *bool  `json:"createdb,omitempty"`
	CreateRole      *bool  `json:"createrole,omitempty"`
	// Config is the desired set of role-level pg_db_role_setting entries
	// (setdatabase = 0). SET param FROM CURRENT stores the sentinel
	// ConfigFromCurrent. RESET removes a key when merging sequential ALTERs.
	Config map[string]string `json:"config,omitempty"`
}

// ConfigFromCurrent is stored in Role.Config when RolesSQL declares
// ALTER ROLE ... SET param FROM CURRENT.
const ConfigFromCurrent = "FROM CURRENT"

// PasswordRedacted is the placeholder rendered in Step.SQL and plan documents
// whenever a role password would otherwise appear in cleartext.
const PasswordRedacted = "********"

// Grant represents a desired privilege grant scanned from SchemaSQL or
// RolesSQL.
type Grant struct {
	Grantee            string   `json:"grantee"`     // role name or PUBLIC
	ObjectKind         string   `json:"object_kind"` // TABLE, SEQUENCE, DATABASE, SCHEMA, FUNCTION
	ObjectName         string   `json:"object_name"` // as scanned; schema-qualified where applicable
	Privileges         []string `json:"privileges"`  // SELECT, INSERT, ... or kind-specific ALL expansion
	GrantOption        bool     `json:"grant_option,omitempty"`
	ExplicitPrivileges []string `json:"-"`
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
	createRoleRe = regexp.MustCompile(`(?i)^CREATE\s+(?:ROLE|USER)\s+` + catalogIdentifierPattern)
	alterRoleRe  = regexp.MustCompile(`(?i)^ALTER\s+(?:ROLE|USER)\s+`)

	// grantRe matches GRANT privilege[, ...] ON [TABLE|SEQUENCE|DATABASE|SCHEMA|FUNCTION]
	// object[, ...] TO grantee[, ...] [WITH GRANT OPTION]. The object slot
	// for FUNCTION keeps its argument list verbatim. Captures: (1) privileges,
	// (2) optional object kind, (3) object list, (4) grantee list,
	// (5) optional "WITH GRANT OPTION".
	grantRe = regexp.MustCompile(`(?is)^GRANT\s+(.+?)\s+ON\s+(?:(TABLE|SEQUENCE|DATABASE|SCHEMA|FUNCTION)\s+)?(.+?)\s+TO\s+(.+?)(\s+WITH\s+GRANT\s+OPTION)?\s*$`)

	// revokeRe matches the desired-state counterpart of grantRe. Captures:
	// (1) optional "GRANT OPTION FOR", (2) privileges, (3) optional object
	// kind, (4) object list, (5) grantee list.
	revokeRe               = regexp.MustCompile(`(?is)^REVOKE\s+(GRANT\s+OPTION\s+FOR\s+)?(.+?)\s+ON\s+(?:(TABLE|SEQUENCE|DATABASE|SCHEMA|FUNCTION)\s+)?(.+?)\s+FROM\s+(.+?)(?:\s+(?:CASCADE|RESTRICT))?\s*$`)
	adminOptionRe          = regexp.MustCompile(`(?is)\s+WITH\s+ADMIN\s+OPTION\s*$`)
	functionObjectRe       = regexp.MustCompile(`(?is)^` + catalogIdentifierPartPattern + `(?:\s*\.\s*` + catalogIdentifierPartPattern + `)?\s*\([^()]*\)$`)
	functionArgumentModeRe = regexp.MustCompile(`(?i)^(?:INOUT|IN|OUT|VARIADIC)\b`)
)

// Privilege sets used to expand ALL per object kind. They mirror the
// PostgreSQL defaults so aclexplode output compares 1:1 with desired state.
var (
	tablePrivileges     = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"}
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
	return strings.ToUpper(strings.TrimSpace(kind)) + ":" +
		CanonicalGrantObject(kind, object, "") + ":" +
		canonicalRoleKey(grantee)
}

func canonicalRoleKey(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if IsPublicRoleIdentifier(identifier) {
		return "public-grantee"
	}
	return "role:" + canonicalRoleName(identifier)
}

func canonicalRoleName(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if len(identifier) >= 2 && identifier[0] == '"' && identifier[len(identifier)-1] == '"' {
		return decodeIdentifier(identifier)
	}
	return strings.ToLower(identifier)
}

// CanonicalRoleName returns the PostgreSQL role identity represented by a
// quoted or unquoted role token.
func CanonicalRoleName(identifier string) string {
	return canonicalRoleName(identifier)
}

// IsPublicRoleIdentifier reports whether a raw SQL role token is the PUBLIC
// keyword. A quoted "PUBLIC" is a distinct role name.
func IsPublicRoleIdentifier(identifier string) bool {
	identifier = strings.TrimSpace(identifier)
	if len(identifier) >= 2 && identifier[0] == '"' && identifier[len(identifier)-1] == '"' {
		return false
	}
	return strings.EqualFold(identifier, "PUBLIC")
}

// CanonicalGrantObject returns the identity key used for ACL comparison.
// Unquoted identifiers are normalized to PostgreSQL's lower-case form while
// quoted identifiers retain their decoded case. Function signatures are
// normalized independently from the function name so ACL inspection and
// RolesSQL use the same identity.
func CanonicalGrantObject(kind, object, targetSchema string) string {
	kind = strings.ToUpper(strings.TrimSpace(kind))
	if kind == "FUNCTION" {
		return canonicalFunctionObject(object, targetSchema)
	}
	parts := splitQualifiedIdentifier(object)
	if len(parts) == 0 {
		return ""
	}
	if (kind == "TABLE" || kind == "SEQUENCE") && len(parts) == 1 && targetSchema != "" {
		parts = append([]string{canonicalTargetIdentifierPart(targetSchema)}, parts...)
	}
	for i, part := range parts {
		parts[i] = canonicalGrantIdentifierPart(part)
	}
	return strings.Join(parts, ".")
}

func canonicalFunctionObject(object, targetSchema string) string {
	object = strings.TrimSpace(object)
	open := -1
	inQuote := false
	for i := 0; i < len(object); i++ {
		switch object[i] {
		case '"':
			if inQuote && i+1 < len(object) && object[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
		case '(':
			if !inQuote {
				open = i
				i = len(object)
			}
		}
	}
	name := object
	args := ""
	if open >= 0 {
		name = strings.TrimSpace(object[:open])
		args = strings.TrimSpace(object[open+1:])
		if strings.HasSuffix(args, ")") {
			args = strings.TrimSpace(args[:len(args)-1])
		}
	}
	nameParts := splitQualifiedIdentifier(name)
	if len(nameParts) == 1 && targetSchema != "" {
		nameParts = append([]string{canonicalTargetIdentifierPart(targetSchema)}, nameParts...)
	}
	for i, part := range nameParts {
		nameParts[i] = canonicalGrantIdentifierPart(part)
	}
	return strings.Join(nameParts, ".") + "(" + canonicalFunctionArguments(args) + ")"
}

func canonicalFunctionArguments(args string) string {
	if strings.TrimSpace(args) == "" {
		return ""
	}
	parts, ok := splitSQLList(args)
	if !ok {
		return normalizeFunctionTypeAliases(strings.ToLower(strings.Join(strings.Fields(args), " ")))
	}
	for i, part := range parts {
		parts[i] = canonicalGrantType(part)
	}
	return strings.Join(parts, ", ")
}

func canonicalGrantType(value string) string {
	var out strings.Builder
	inQuote := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			if inQuote && i+1 < len(value) && value[i+1] == '"' {
				out.WriteString(`""`)
				i++
				continue
			}
			inQuote = !inQuote
			out.WriteByte(value[i])
		default:
			if inQuote {
				out.WriteByte(value[i])
			} else {
				out.WriteString(strings.ToLower(string(value[i])))
			}
		}
	}
	return normalizeFunctionTypeAliases(strings.Join(strings.Fields(out.String()), " "))
}

// canonicalTargetIdentifierPart canonicalizes a target schema supplied by
// configuration. Target schemas are catalog identities rather than SQL
// tokens, so mixed-case and punctuation-bearing names must remain quoted
// instead of being folded as an unquoted SQL identifier.
func canonicalTargetIdentifierPart(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if len(identifier) >= 2 && identifier[0] == '"' && identifier[len(identifier)-1] == '"' {
		return canonicalGrantIdentifierPart(identifier)
	}
	if isSimpleLowerIdentifier(identifier) {
		return strings.ToLower(identifier)
	}
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

var functionTypeAliases = map[string]string{
	"int":               "integer",
	"int2":              "smallint",
	"int4":              "integer",
	"int8":              "bigint",
	"float4":            "real",
	"float8":            "double precision",
	"bool":              "boolean",
	"decimal":           "numeric",
	"varchar":           "character varying",
	"varbit":            "bit varying",
	"timestamptz":       "timestamp with time zone",
	"timetz":            "time with time zone",
	"timestamp with tz": "timestamp with time zone",
	"time with tz":      "time with time zone",
	"double":            "double precision",
}

func normalizeFunctionTypeAliases(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "[ ]", "[]")
	value = strings.ReplaceAll(value, " [", "[")
	value = strings.ReplaceAll(value, "] ", "]")

	var arraySuffix strings.Builder
	for strings.HasSuffix(value, "[]") {
		arraySuffix.WriteString("[]")
		value = strings.TrimSpace(strings.TrimSuffix(value, "[]"))
	}
	value = strings.TrimPrefix(value, "pg_catalog.")
	if alias, ok := functionTypeAliases[value]; ok {
		value = alias
	}
	return value + arraySuffix.String()
}

func canonicalGrantIdentifierPart(part string) string {
	part = strings.TrimSpace(part)
	if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
		decoded := strings.ReplaceAll(part[1:len(part)-1], `""`, `"`)
		if decoded != "" && decoded == strings.ToLower(decoded) &&
			catalogIdentifierPartRe.MatchString(decoded) {
			return decoded
		}
		return `"` + strings.ReplaceAll(decoded, `"`, `""`) + `"`
	}
	return strings.ToLower(part)
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
			role, err := parseCreateRoleStatement(trimmed)
			if err == nil && role != nil {
				key := CanonicalIdentifierKey(role.Name)
				if existing := spec.Roles[key]; existing != nil {
					mergeRoleAttrs(existing, role)
				} else {
					spec.Roles[key] = role
				}
			}
		case strings.HasPrefix(upper, "ALTER ROLE") || strings.HasPrefix(upper, "ALTER USER"):
			name, partial, resetKeys, err := parseAlterRoleStatement(trimmed)
			if err == nil && name != "" {
				key := CanonicalIdentifierKey(name)
				role := spec.Roles[key]
				if role == nil {
					role = &Role{Name: name}
					spec.Roles[key] = role
				}
				applyAlterToRole(role, partial, resetKeys)
			}
		case strings.HasPrefix(upper, "GRANT"):
			spec.Grants = append(spec.Grants, parseGrantStatement(trimmed)...)
		case strings.HasPrefix(upper, "REVOKE"):
			revokes = append(revokes, parseRevokeStatement(trimmed)...)
		}
	}
	applyRevokes(spec, revokes, "")
	spec.revokes = revokes
	return spec
}

// MergeRolesSpecs combines role desired state from SchemaSQL with the
// optional RolesSQL side-channel. Entries from the side-channel replace
// SchemaSQL entries with the same role or grant identity, making the explicit
// side-channel authoritative on duplicates.
func MergeRolesSpecs(schemaSpec, sideSpec *RolesSpec) *RolesSpec {
	return MergeRolesSpecsForTarget(schemaSpec, sideSpec, "")
}

// MergeRolesSpecsForTarget combines role desired state while resolving
// unqualified TABLE/SEQUENCE grant targets against targetSchema. The
// side-channel remains authoritative when both sources describe the same
// canonical grant target.
func MergeRolesSpecsForTarget(schemaSpec, sideSpec *RolesSpec, targetSchema string) *RolesSpec {
	out := &RolesSpec{Roles: make(map[string]*Role)}
	for _, spec := range []*RolesSpec{schemaSpec, sideSpec} {
		if spec == nil {
			continue
		}
		for key, role := range spec.Roles {
			// Side-channel (second pass) replaces schema entries wholesale,
			// carrying all attributes so LOGIN/PASSWORD/SET from RolesSQL win.
			out.Roles[key] = cloneRole(role)
		}
	}

	schemaGrants := schemaSpec
	if schemaSpec != nil && len(schemaSpec.revokes) > 0 {
		schemaGrants = &RolesSpec{Grants: make([]*Grant, 0, len(schemaSpec.Grants))}
		for _, grant := range schemaSpec.Grants {
			schemaGrants.Grants = append(schemaGrants.Grants, cloneGrant(grant))
		}
		applyRevokes(schemaGrants, schemaSpec.revokes, targetSchema)
	}
	out.Grants = mergeGrantSpecs(schemaGrants, sideSpec, targetSchema)
	if sideSpec != nil {
		applyRevokes(out, sideSpec.revokes, targetSchema)
	}
	if schemaSpec != nil {
		out.revokes = append(out.revokes, schemaSpec.revokes...)
	}
	if sideSpec != nil {
		out.revokes = append(out.revokes, sideSpec.revokes...)
	}
	return out
}

// ValidateRolesSpecScope rejects ACL object names that InspectLiveRoles does
// not inventory. Keeping this contract explicit prevents valid-looking
// grants from becoming invisible desired state and drifting forever.
func ValidateRolesSpecScope(spec *RolesSpec, targetSchemas []string, currentDatabase string) error {
	if spec == nil {
		return nil
	}
	targets := make(map[string]bool, len(targetSchemas))
	for _, targetSchema := range targetSchemas {
		targets[CanonicalTargetIdentifier(targetSchema)] = true
	}
	currentDatabase = CanonicalTargetIdentifier(currentDatabase)
	entries := make([]*Grant, 0, len(spec.Grants)+len(spec.revokes))
	entries = append(entries, spec.Grants...)
	entries = append(entries, spec.revokes...)
	for _, grant := range entries {
		kind := strings.ToUpper(strings.TrimSpace(grant.ObjectKind))
		parts := grantObjectNameParts(kind, grant.ObjectName)
		if len(parts) == 0 {
			return fmt.Errorf("%s grant has an empty object name", kind)
		}
		var objectSchema string
		switch kind {
		case "TABLE", "SEQUENCE", "FUNCTION":
			if len(parts) > 2 {
				return fmt.Errorf("%s grant object %q has too many qualification levels", kind, grant.ObjectName)
			}
			if len(parts) == 2 {
				objectSchema = CanonicalIdentifierPart(parts[0])
			} else if len(targetSchemas) == 0 {
				return fmt.Errorf("%s grant object %q is unqualified but no target schema is configured", kind, grant.ObjectName)
			} else {
				objectSchema = CanonicalTargetIdentifier(targetSchemas[0])
			}
			if !targets[objectSchema] {
				return fmt.Errorf("%s grant object %q is outside the configured target schemas", kind, grant.ObjectName)
			}
		case "SCHEMA":
			if len(parts) != 1 {
				return fmt.Errorf("SCHEMA grant object %q must be an unqualified schema name", grant.ObjectName)
			}
			objectSchema = CanonicalIdentifierPart(parts[0])
			if !targets[objectSchema] {
				return fmt.Errorf("SCHEMA grant object %q is outside the configured target schemas", grant.ObjectName)
			}
		case "DATABASE":
			if len(parts) != 1 {
				return fmt.Errorf("DATABASE grant object %q must be an unqualified database name", grant.ObjectName)
			}
			if currentDatabase == "" || CanonicalIdentifierPart(parts[0]) != currentDatabase {
				return fmt.Errorf("DATABASE grant object %q is outside the current database ACL scope", grant.ObjectName)
			}
		default:
			return fmt.Errorf("unsupported ACL object kind %q", grant.ObjectKind)
		}
	}
	return nil
}

func grantObjectNameParts(kind, object string) []string {
	if kind == "FUNCTION" {
		object = functionObjectName(object)
	}
	return splitQualifiedIdentifier(object)
}

func functionObjectName(object string) string {
	inQuote := false
	for i := 0; i < len(object); i++ {
		switch object[i] {
		case '"':
			if inQuote && i+1 < len(object) && object[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
		case '(':
			if !inQuote {
				return strings.TrimSpace(object[:i])
			}
		}
	}
	return strings.TrimSpace(object)
}

// FilterRolePrivilegesForServer removes privileges that are not understood by
// the connected PostgreSQL version. MAINTAIN was added in PostgreSQL 17 and
// is included in ALL TABLE expansion, but an explicit MAINTAIN request must
// fail rather than being silently discarded.
func FilterRolePrivilegesForServer(spec *RolesSpec, serverVersion int) (*RolesSpec, error) {
	if spec == nil || serverVersion == 0 || serverVersion >= 170000 {
		return spec, nil
	}
	for _, revoke := range spec.revokes {
		if containsFold(revoke.ExplicitPrivileges, "MAINTAIN") {
			return nil, fmt.Errorf("MAINTAIN is supported only by PostgreSQL 17 or later")
		}
	}
	filteredGrants := spec.Grants[:0]
	for _, grant := range spec.Grants {
		filtered := grant.Privileges[:0]
		for _, privilege := range grant.Privileges {
			if strings.EqualFold(privilege, "MAINTAIN") {
				if containsFold(grant.ExplicitPrivileges, "MAINTAIN") {
					return nil, fmt.Errorf("MAINTAIN is supported only by PostgreSQL 17 or later")
				}
				continue
			}
			filtered = append(filtered, privilege)
		}
		grant.Privileges = filtered
		if len(grant.Privileges) > 0 {
			filteredGrants = append(filteredGrants, grant)
		}
	}
	spec.Grants = filteredGrants
	return spec, nil
}

func mergeGrantSpecs(schemaSpec, sideSpec *RolesSpec, targetSchema string) []*Grant {
	var out []*Grant
	for _, spec := range []*RolesSpec{schemaSpec, sideSpec} {
		if spec == nil {
			continue
		}
		var next []*Grant
		for _, grant := range spec.Grants {
			replaced := false
			for i, existing := range next {
				if sameGrantTargetForSchema(existing, grant, targetSchema) {
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
					if sameGrantTargetForSchema(existing, grant, targetSchema) {
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
	for _, privilege := range b.ExplicitPrivileges {
		if !containsFold(out.ExplicitPrivileges, privilege) {
			out.ExplicitPrivileges = append(out.ExplicitPrivileges, privilege)
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
		Grantee:            g.Grantee,
		ObjectKind:         g.ObjectKind,
		ObjectName:         g.ObjectName,
		Privileges:         slicesCloneStrings(g.Privileges),
		GrantOption:        g.GrantOption,
		ExplicitPrivileges: slicesCloneStrings(g.ExplicitPrivileges),
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
	explicitPrivileges := explicitPrivileges(privList)
	if len(privileges) == 0 || len(objectList) == 0 || len(granteeList) == 0 {
		return nil
	}

	var out []*Grant
	for _, object := range objectList {
		for _, grantee := range granteeList {
			grantee = strings.TrimSpace(grantee)
			if !validCatalogIdentifierPart(grantee) {
				continue
			}
			out = append(out, &Grant{
				Grantee:            grantee,
				ObjectKind:         kind,
				ObjectName:         object,
				Privileges:         slicesCloneStrings(privileges),
				GrantOption:        grantOption,
				ExplicitPrivileges: slicesCloneStrings(explicitPrivileges),
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
	explicitPrivileges := explicitPrivileges(privList)
	if len(privileges) == 0 || len(objectList) == 0 || len(granteeList) == 0 {
		return nil
	}

	var out []*Grant
	for _, object := range objectList {
		for _, grantee := range granteeList {
			grantee = strings.TrimSpace(grantee)
			if !validCatalogIdentifierPart(grantee) {
				continue
			}
			out = append(out, &Grant{
				Grantee:            grantee,
				ObjectKind:         kind,
				ObjectName:         object,
				Privileges:         slicesCloneStrings(privileges),
				GrantOption:        grantOption,
				ExplicitPrivileges: slicesCloneStrings(explicitPrivileges),
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

func explicitPrivileges(privList string) []string {
	var privileges []string
	for _, privilege := range splitList(privList) {
		upper := strings.ToUpper(strings.TrimSpace(privilege))
		if upper == "ALL" || upper == "ALL PRIVILEGES" || upper == "" {
			continue
		}
		privileges = append(privileges, upper)
	}
	return dedupeStrings(privileges)
}

func applyRevokes(spec *RolesSpec, revokes []*Grant, targetSchema string) {
	if len(revokes) == 0 || len(spec.Grants) == 0 {
		return
	}

	for _, revoke := range revokes {
		var kept []*Grant
		for _, grant := range spec.Grants {
			if !sameGrantTargetForSchema(grant, revoke, targetSchema) {
				kept = append(kept, grant)
				continue
			}
			if revoke.GrantOption {
				// PR1 rejects this syntax in ValidateRolesSQL because Grant
				// only carries one option bit for the whole privilege set.
				// Keep parsing defensive: never clear the option for an
				// unrelated subset that the IR cannot represent precisely.
				if samePrivilegeSet(grant.Privileges, revoke.Privileges) {
					grant.GrantOption = false
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
			explicitRemaining := grant.ExplicitPrivileges[:0]
			for _, privilege := range grant.ExplicitPrivileges {
				if !containsFold(revoke.Privileges, privilege) {
					explicitRemaining = append(explicitRemaining, privilege)
				}
			}
			grant.ExplicitPrivileges = explicitRemaining
			if len(grant.Privileges) > 0 {
				kept = append(kept, grant)
			}
		}
		spec.Grants = kept
	}
}

func samePrivilegeSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, privilege := range a {
		if !containsFold(b, privilege) {
			return false
		}
	}
	return true
}

func sameGrantTargetForSchema(a, b *Grant, targetSchema string) bool {
	return strings.EqualFold(a.ObjectKind, b.ObjectKind) &&
		CanonicalGrantObject(a.ObjectKind, a.ObjectName, targetSchema) ==
			CanonicalGrantObject(b.ObjectKind, b.ObjectName, targetSchema) &&
		canonicalRoleKey(a.Grantee) == canonicalRoleKey(b.Grantee)
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

// splitList splits a comma-separated SQL list, respecting quoted strings and
// function argument parentheses. Invalid lists return nil.
func splitList(s string) []string {
	parts, ok := splitSQLList(s)
	if !ok {
		return nil
	}
	return parts
}

func splitSQLList(s string) ([]string, bool) {
	var parts []string
	var cur strings.Builder
	inDoubleQuote := false
	inSingleQuote := false
	parentheses := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			cur.WriteByte(s[i])
			if inDoubleQuote && i+1 < len(s) && s[i+1] == '"' {
				cur.WriteByte(s[i+1])
				i++
				continue
			}
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
		case '\'':
			cur.WriteByte(s[i])
			if inSingleQuote && i+1 < len(s) && s[i+1] == '\'' {
				cur.WriteByte(s[i+1])
				i++
				continue
			}
			if !inDoubleQuote {
				inSingleQuote = !inSingleQuote
			}
		case '(':
			cur.WriteByte(s[i])
			if !inDoubleQuote && !inSingleQuote {
				parentheses++
			}
		case ')':
			cur.WriteByte(s[i])
			if !inDoubleQuote && !inSingleQuote {
				if parentheses == 0 {
					return nil, false
				}
				parentheses--
			}
		case ',':
			if inDoubleQuote || inSingleQuote || parentheses > 0 {
				cur.WriteByte(s[i])
				continue
			}
			item := strings.TrimSpace(cur.String())
			if item == "" {
				return nil, false
			}
			parts = append(parts, item)
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	if inDoubleQuote || inSingleQuote || parentheses != 0 {
		return nil, false
	}
	tail := strings.TrimSpace(cur.String())
	if tail == "" {
		return nil, false
	}
	return append(parts, tail), true
}

func validateGrantLists(matches []string, revoke bool) error {
	offset := 0
	if revoke {
		offset = 1
	}
	privileges := strings.TrimSpace(matches[1+offset])
	objects := strings.TrimSpace(matches[3+offset])
	grantees := strings.TrimSpace(matches[4+offset])
	if privileges == "" {
		return fmt.Errorf("privilege list is empty")
	}
	if adminOptionRe.MatchString(grantees) {
		return fmt.Errorf("WITH ADMIN OPTION is not supported")
	}
	privilegeList, ok := splitSQLList(privileges)
	if !ok {
		return fmt.Errorf("privilege list is malformed")
	}
	for _, privilege := range privilegeList {
		upper := strings.ToUpper(strings.TrimSpace(privilege))
		if upper == "" {
			return fmt.Errorf("privilege list contains an empty item")
		}
		if upper != "ALL" && upper != "ALL PRIVILEGES" &&
			!validCatalogIdentifierPart(upper) {
			return fmt.Errorf("privilege %q is malformed", privilege)
		}
	}
	objectList, ok := splitSQLList(objects)
	if !ok {
		return fmt.Errorf("object list is malformed")
	}
	kind := strings.ToUpper(strings.TrimSpace(matches[2+offset]))
	if kind == "" {
		kind = "TABLE"
	}
	for _, object := range objectList {
		if kind == "FUNCTION" {
			if err := validateFunctionObject(strings.TrimSpace(object)); err != nil {
				return fmt.Errorf("object list contains malformed item %q: %w", object, err)
			}
			continue
		}
		if !validRoleObject(strings.TrimSpace(object), kind == "TABLE" || kind == "SEQUENCE") {
			return fmt.Errorf("object list contains malformed item %q", object)
		}
	}
	granteeList, ok := splitSQLList(grantees)
	if !ok {
		return fmt.Errorf("grantee list is malformed")
	}
	for _, grantee := range granteeList {
		if !validCatalogIdentifierPart(grantee) {
			return fmt.Errorf("grantee list contains malformed item %q", grantee)
		}
	}
	return nil
}

func validateFunctionObject(object string) error {
	name, args, ok := splitFunctionObjectIdentity(object)
	if !ok {
		return fmt.Errorf("FUNCTION grants require a schema/name followed by an identity argument list")
	}
	if !functionObjectRe.MatchString(object) {
		return fmt.Errorf("FUNCTION name is malformed")
	}
	for _, part := range splitQualifiedIdentifier(name) {
		if !validCatalogIdentifierPart(part) {
			return fmt.Errorf("FUNCTION name contains an invalid identifier")
		}
	}
	if args == "" {
		return nil
	}
	parts, ok := splitSQLList(args)
	if !ok {
		return fmt.Errorf("FUNCTION identity argument list is malformed")
	}
	for _, part := range parts {
		if err := validateFunctionIdentityArgument(part); err != nil {
			return err
		}
	}
	return nil
}

func splitFunctionObjectIdentity(object string) (name, args string, ok bool) {
	object = strings.TrimSpace(object)
	inQuote := false
	for i := 0; i < len(object); i++ {
		switch object[i] {
		case '"':
			if inQuote && i+1 < len(object) && object[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
		case '(':
			if inQuote {
				continue
			}
			if !strings.HasSuffix(object, ")") {
				return "", "", false
			}
			return strings.TrimSpace(object[:i]), strings.TrimSpace(object[i+1 : len(object)-1]), true
		}
	}
	return "", "", false
}

func validateFunctionIdentityArgument(argument string) error {
	argument = strings.TrimSpace(argument)
	if argument == "" {
		return fmt.Errorf("FUNCTION identity argument is empty")
	}
	if functionArgumentModeRe.MatchString(argument) {
		return fmt.Errorf("FUNCTION grants accept identity types only; argument modes are unsupported")
	}
	fields := strings.Fields(argument)
	if len(fields) == 1 {
		return nil
	}
	lower := strings.ToLower(strings.Join(fields, " "))
	for _, prefix := range []string{
		"bit varying",
		"character varying",
		"double precision",
		"interval",
		"time with time zone",
		"time without time zone",
		"timestamp with time zone",
		"timestamp without time zone",
	} {
		if lower == prefix || strings.HasPrefix(lower, prefix+" ") {
			return nil
		}
	}
	return fmt.Errorf("FUNCTION grants accept identity types only; argument names are unsupported")
}

func validRoleObject(object string, qualified bool) bool {
	parts := splitQualifiedIdentifier(object)
	maxParts := 1
	if qualified {
		maxParts = 2
	}
	if len(parts) == 0 || len(parts) > maxParts {
		return false
	}
	for _, part := range parts {
		if !validCatalogIdentifierPart(part) {
			return false
		}
	}
	return true
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
// declarations (including LOGIN/PASSWORD/config attributes), ALTER ROLE
// WITH/SET/RESET, and object-privilege GRANT/REVOKE statements are accepted;
// unsupported forms fail loudly instead of being silently ignored.
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
		case strings.HasPrefix(upper, "CREATE ROLE"), strings.HasPrefix(upper, "CREATE USER"):
			if _, err := parseCreateRoleStatement(trimmed); err != nil {
				return fmt.Errorf("unsupported role declaration in RolesSQL: %w: %q", err, trimmed)
			}
		case strings.HasPrefix(upper, "ALTER ROLE"), strings.HasPrefix(upper, "ALTER USER"):
			if !alterRoleRe.MatchString(trimmed) {
				return fmt.Errorf("unsupported ALTER ROLE/USER form in RolesSQL: %q", trimmed)
			}
			if _, _, _, err := parseAlterRoleStatement(trimmed); err != nil {
				return fmt.Errorf("unsupported ALTER ROLE/USER form in RolesSQL: %w: %q", err, trimmed)
			}
		case strings.HasPrefix(upper, "DROP ROLE"), strings.HasPrefix(upper, "DROP USER"):
			return fmt.Errorf("DROP ROLE/USER is not supported in the declarative role contract: %q", trimmed)
		case strings.HasPrefix(upper, "GRANT"):
			matches := grantRe.FindStringSubmatch(trimmed)
			if matches == nil {
				return fmt.Errorf("unsupported GRANT form in RolesSQL (expected GRANT <privileges> ON [KIND] <object> TO <grantee>): %q", trimmed)
			}
			if err := validateGrantLists(matches, false); err != nil {
				return fmt.Errorf("unsupported GRANT form in RolesSQL: %w: %q", err, trimmed)
			}
		case strings.HasPrefix(upper, "REVOKE"):
			matches := revokeRe.FindStringSubmatch(trimmed)
			if matches == nil {
				return fmt.Errorf("unsupported REVOKE form in RolesSQL (expected REVOKE <privileges> ON [KIND] <object> FROM <grantee>): %q", trimmed)
			}
			if strings.TrimSpace(matches[1]) != "" {
				return fmt.Errorf("per-privilege grant-option revocation is not supported in PR1: %q", trimmed)
			}
			if err := validateGrantLists(matches, true); err != nil {
				return fmt.Errorf("unsupported REVOKE form in RolesSQL: %w: %q", err, trimmed)
			}
		default:
			return fmt.Errorf("unsupported statement in RolesSQL (only CREATE/ALTER ROLE/USER and GRANT/REVOKE are managed): %q", trimmed)
		}
	}
	return nil
}

// FormatGrantKey is a stable display key for grant records.
func FormatGrantKey(g *Grant) string {
	return fmt.Sprintf("%s:%s:%s", g.ObjectKind, g.ObjectName, g.Grantee)
}
