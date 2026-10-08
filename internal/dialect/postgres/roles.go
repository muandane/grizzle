package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/schema"
)

// LiveRoles captures the role/privilege state relevant to a RolesSQL diff.
type LiveRoles struct {
	// RoleNames is every non-system role name in the database, keyed by exact
	// PostgreSQL role identity.
	RoleNames map[string]string
	// ManagedRoles are roles stamped with the grizzle-managed catalog comment.
	ManagedRoles map[string]bool
	// Grants maps schema.GrantKey(kind, object, grantee) -> privileges with
	// grant option tracking. Object names retain quoted identity components.
	Grants map[string]*LiveGrant
}

// LiveGrant is one live (grantee, object) ACL entry. Only explicitly granted
// privileges appear: relacl/nspacl/datacl are NULL when nothing was ever
// granted, and implicit owner privileges are not ACL rows — both are skipped.
type LiveGrant struct {
	Grantee     string
	ObjectKind  string
	ObjectName  string
	Privileges  map[string]bool // privilege_type -> present
	GrantOption map[string]bool // privilege_type -> is_grantable
}

// InspectLiveRoles reads managed-role markers and object ACLs. Tables and
// sequences are scoped to the target schemas; schema ACLs cover the target
// schemas; the database ACL covers the current database.
func InspectLiveRoles(ctx context.Context, dbtx dialect.DBTX, targetSchemas []string) (*LiveRoles, error) {
	live := &LiveRoles{
		RoleNames:    make(map[string]string),
		ManagedRoles: make(map[string]bool),
		Grants:       make(map[string]*LiveGrant),
	}

	// 1. Roles: all non-system roles plus the managed marker comment.
	// Role comments live in pg_shdescription (obj_description does not see
	// shared-object comments), keyed by the pg_authid catalog class.
	roleRows, err := dbtx.QueryContext(ctx, `
		SELECT r.rolname,
		       COALESCE((SELECT d.description
		                 FROM pg_shdescription d
		                 WHERE d.objoid = r.oid
		                   AND d.classoid = 'pg_authid'::regclass
		                ), '') AS comment
		FROM pg_roles r
		WHERE r.rolname NOT LIKE 'pg\_%';
	`)
	if err != nil {
		return nil, fmt.Errorf("inspecting roles: %w", err)
	}
	if err := scanRows(roleRows, func(scan func(...any) error) error {
		var name, comment string
		if err := scan(&name, &comment); err != nil {
			return fmt.Errorf("scanning roles: %w", err)
		}
		live.RoleNames[schema.CanonicalIdentifierKey(name)] = name
		if strings.Contains(comment, schema.RoleManagedComment) {
			live.ManagedRoles[schema.CanonicalIdentifierKey(name)] = true
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// 2. Table + sequence ACLs scoped to target schemas. INNER join on
	// aclexplode skips NULL relacl objects (no explicit grants to manage).
	schemaList, args := placeholders(targetSchemas)
	aclQuery := fmt.Sprintf(`
		SELECT n.nspname, c.relname, c.relkind,
		       a.grantee = 0 AS is_public, COALESCE(r.rolname, '') AS rolname,
		       a.privilege_type, a.is_grantable
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN aclexplode(c.relacl) a ON true
		LEFT JOIN pg_roles r ON r.oid = a.grantee
		WHERE c.relkind IN ('r', 'p', 'S')
		  AND n.nspname IN (%s);
	`, schemaList)
	aclRows, err := dbtx.QueryContext(ctx, aclQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("inspecting table ACLs: %w", err)
	}
	if err := scanRows(aclRows, func(scan func(...any) error) error {
		var schemaName, objectName, relKind, roleName, priv string
		var isPublic bool
		var grantable bool
		if err := scan(&schemaName, &objectName, &relKind, &isPublic, &roleName, &priv, &grantable); err != nil {
			return fmt.Errorf("scanning table ACLs: %w", err)
		}
		kind := "TABLE"
		if relKind == "S" {
			kind = "SEQUENCE"
		}
		recordLiveGrant(live, kind, []string{schemaName, objectName}, "", isPublic, roleName, priv, grantable)
		return nil
	}); err != nil {
		return nil, err
	}

	// 3. Function ACLs scoped to target schemas. Function identity includes
	// identity arguments because PostgreSQL permits overloaded functions.
	functionACLQuery := fmt.Sprintf(`
		SELECT n.nspname,
		       p.proname,
		       pg_get_function_identity_arguments(p.oid),
		       a.grantee = 0 AS is_public, COALESCE(r.rolname, '') AS rolname,
		       a.privilege_type, a.is_grantable
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		JOIN aclexplode(p.proacl) a ON true
		LEFT JOIN pg_roles r ON r.oid = a.grantee
		WHERE n.nspname IN (%s)
		  AND p.prokind = 'f';
	`, schemaList)
	functionACLRows, err := dbtx.QueryContext(ctx, functionACLQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("inspecting function ACLs: %w", err)
	}
	if err := scanRows(functionACLRows, func(scan func(...any) error) error {
		var schemaName, functionName, identityArgs, roleName, priv string
		var isPublic bool
		var grantable bool
		if err := scan(&schemaName, &functionName, &identityArgs, &isPublic, &roleName, &priv, &grantable); err != nil {
			return fmt.Errorf("scanning function ACLs: %w", err)
		}
		recordLiveGrant(live, "FUNCTION",
			[]string{schemaName, functionName}, identityArgs, isPublic, roleName, priv, grantable)
		return nil
	}); err != nil {
		return nil, err
	}

	// 4. Schema ACLs for target schemas.
	schemaACLQuery := fmt.Sprintf(`
		SELECT n.nspname,
		       a.grantee = 0 AS is_public, COALESCE(r.rolname, '') AS rolname,
		       a.privilege_type, a.is_grantable
		FROM pg_namespace n
		JOIN aclexplode(n.nspacl) a ON true
		LEFT JOIN pg_roles r ON r.oid = a.grantee
		WHERE n.nspname IN (%s);
	`, schemaList)
	schemaACLRows, err := dbtx.QueryContext(ctx, schemaACLQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("inspecting schema ACLs: %w", err)
	}
	if err := scanRows(schemaACLRows, func(scan func(...any) error) error {
		var schemaName, roleName, priv string
		var isPublic bool
		var grantable bool
		if err := scan(&schemaName, &isPublic, &roleName, &priv, &grantable); err != nil {
			return fmt.Errorf("scanning schema ACLs: %w", err)
		}
		recordLiveGrant(live, "SCHEMA", []string{schemaName}, "", isPublic, roleName, priv, grantable)
		return nil
	}); err != nil {
		return nil, err
	}

	// 5. Database ACLs (current database only).
	dbACLRows, err := dbtx.QueryContext(ctx, `
		SELECT d.datname,
		       a.grantee = 0 AS is_public, COALESCE(r.rolname, '') AS rolname,
		       a.privilege_type, a.is_grantable
		FROM pg_database d
		JOIN aclexplode(d.datacl) a ON true
		LEFT JOIN pg_roles r ON r.oid = a.grantee
		WHERE d.datname = current_database();
	`)
	if err != nil {
		return nil, fmt.Errorf("inspecting database ACLs: %w", err)
	}
	if err := scanRows(dbACLRows, func(scan func(...any) error) error {
		var dbName, roleName, priv string
		var isPublic bool
		var grantable bool
		if err := scan(&dbName, &isPublic, &roleName, &priv, &grantable); err != nil {
			return fmt.Errorf("scanning database ACLs: %w", err)
		}
		recordLiveGrant(live, "DATABASE", []string{dbName}, "", isPublic, roleName, priv, grantable)
		return nil
	}); err != nil {
		return nil, err
	}

	return live, nil
}

// scanRows iterates a *sql.Rows with a per-row callback, closing on all paths.
func scanRows(rows *sql.Rows, fn func(scan func(...any) error) error) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// placeholders builds "$1, $2, ..." from a string list.
func placeholders(values []string) (string, []any) {
	parts := make([]string, 0, len(values))
	args := make([]any, 0, len(values))
	for i, v := range values {
		parts = append(parts, fmt.Sprintf("$%d", i+1))
		args = append(args, v)
	}
	return strings.Join(parts, ", "), args
}

// recordLiveGrant folds one aclexplode row into the live grant map.
func recordLiveGrant(live *LiveRoles, kind string, objectParts []string, functionArgs string, isPublic bool, roleName, priv string, grantable bool) {
	if (!isPublic && roleName == "") || priv == "" {
		return
	}
	displayObject := canonicalLiveGrantObjectParts(kind, objectParts, functionArgs)
	canonicalObject := schema.CanonicalGrantObject(kind, displayObject, "")
	granteeToken := canonicalLiveRoleToken(roleName, isPublic)
	key := schema.GrantKey(kind, canonicalObject, granteeToken)
	g := live.Grants[key]
	if g == nil {
		g = &LiveGrant{
			Grantee:     granteeToken,
			ObjectKind:  kind,
			ObjectName:  displayObject,
			Privileges:  make(map[string]bool),
			GrantOption: make(map[string]bool),
		}
		live.Grants[key] = g
	}
	g.Privileges[strings.ToUpper(priv)] = true
	if grantable {
		g.GrantOption[strings.ToUpper(priv)] = true
	}
}

func canonicalLiveRoleToken(name string, isPublic bool) string {
	if isPublic {
		return "PUBLIC"
	}
	name = strings.TrimSpace(name)
	if name == "public" {
		return quoteIdentifier(name)
	}
	if name == strings.ToLower(name) && schema.CanonicalRoleName(name) == name {
		return name
	}
	return quoteIdentifier(name)
}

func canonicalLiveGrantObject(kind, object string) string {
	kind = strings.ToUpper(strings.TrimSpace(kind))
	if kind == "FUNCTION" {
		name, suffix, ok := splitFunctionObject(object)
		if !ok {
			return canonicalLiveGrantObjectParts(kind, splitQualifiedIdentifier(object), "")
		}
		args := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(suffix, "("), ")"))
		if args == suffix {
			args = ""
		}
		return canonicalLiveGrantObjectParts(kind, splitQualifiedIdentifier(name), args)
	}
	return canonicalLiveGrantObjectParts(kind, splitQualifiedIdentifier(object), "")
}

func canonicalLiveGrantObjectParts(kind string, parts []string, functionArgs string) string {
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
			part = strings.ReplaceAll(part[1:len(part)-1], `""`, `"`)
		}
		parts[i] = canonicalLiveIdentifier(part)
	}
	object := strings.Join(parts, ".")
	if strings.EqualFold(kind, "FUNCTION") {
		return object + "(" + strings.TrimSpace(functionArgs) + ")"
	}
	return object
}

// RoleOwnsObjects reports whether the named role owns any cluster object
// that makes DROP ROLE unsafe (databases, schemas, tables, sequences,
// functions, types, languages, or publications).
func RoleOwnsObjects(ctx context.Context, dbtx dialect.DBTX, roleName string) (bool, error) {
	var owns bool
	err := dbtx.QueryRowContext(ctx, `
		SELECT (
			(SELECT count(*) FROM pg_database    WHERE datdba   = r.oid) +
			(SELECT count(*) FROM pg_namespace   WHERE nspowner = r.oid) +
			(SELECT count(*) FROM pg_class       WHERE relowner = r.oid) +
			(SELECT count(*) FROM pg_type        WHERE typowner = r.oid) +
			(SELECT count(*) FROM pg_proc        WHERE proowner = r.oid) +
			(SELECT count(*) FROM pg_language    WHERE lanowner = r.oid) +
			(SELECT count(*) FROM pg_publication WHERE pubowner = r.oid)
		) > 0
		FROM pg_roles r WHERE r.rolname = $1;
	`, roleName).Scan(&owns)
	if err != nil {
		return false, fmt.Errorf("checking ownership for role %q: %w", roleName, err)
	}
	return owns, nil
}

// GenerateCreateRoleSQL renders the NOLOGIN group role Grizzle manages.
func GenerateCreateRoleSQL(roleName string) string {
	return fmt.Sprintf("CREATE ROLE %s NOLOGIN;", quoteIdentifier(roleName))
}

// GenerateRoleCommentSQL stamps the managed-role marker used to distinguish
// Grizzle-owned roles from operator-created ones.
func GenerateRoleCommentSQL(roleName string) string {
	return fmt.Sprintf("COMMENT ON ROLE %s IS '%s';", quoteIdentifier(roleName), schema.RoleManagedComment)
}

// GenerateDropRoleSQL renders DROP ROLE.
func GenerateDropRoleSQL(roleName string) string {
	return fmt.Sprintf("DROP ROLE %s;", quoteIdentifier(roleName))
}

// GenerateGrantSQL renders a GRANT statement from desired IR.
func GenerateGrantSQL(g *schema.Grant) string {
	suffix := ""
	if g.GrantOption {
		suffix = " WITH GRANT OPTION"
	}
	return fmt.Sprintf("GRANT %s ON %s %s TO %s%s;",
		strings.Join(g.Privileges, ", "),
		g.ObjectKind,
		renderGrantObject(g.ObjectKind, g.ObjectName),
		renderRoleIdentifier(g.Grantee),
		suffix)
}

// GenerateRevokeSQL renders a REVOKE statement. A grant IR flagged
// GrantOption revokes only the delegation (REVOKE GRANT OPTION FOR),
// keeping the base privilege.
func GenerateRevokeSQL(g *schema.Grant, privileges []string) string {
	privs := privileges
	if len(privs) == 0 {
		privs = []string{"ALL PRIVILEGES"}
	}
	if g.GrantOption {
		return fmt.Sprintf("REVOKE GRANT OPTION FOR %s ON %s %s FROM %s;",
			strings.Join(privs, ", "),
			g.ObjectKind,
			renderGrantObject(g.ObjectKind, g.ObjectName),
			renderRoleIdentifier(g.Grantee))
	}
	return fmt.Sprintf("REVOKE %s ON %s %s FROM %s;",
		strings.Join(privs, ", "),
		g.ObjectKind,
		renderGrantObject(g.ObjectKind, g.ObjectName),
		renderRoleIdentifier(g.Grantee))
}

func renderRoleIdentifier(identifier string) string {
	if schema.IsPublicRoleIdentifier(identifier) {
		return "PUBLIC"
	}
	return quoteIdentifier(schema.CanonicalRoleName(identifier))
}

func renderGrantObject(kind, object string) string {
	if strings.EqualFold(strings.TrimSpace(kind), "FUNCTION") {
		if name, suffix, ok := splitFunctionObject(object); ok {
			return quoteQualifiedIdentifier(name) + suffix
		}
	}
	return quoteQualifiedIdentifier(object)
}

func splitFunctionObject(value string) (name, suffix string, ok bool) {
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
				return strings.TrimSpace(value[:i]), strings.TrimSpace(value[i:]), true
			}
		}
	}
	return value, "", false
}

// RoleState adapts the inspected live state to the dialect-independent
// diff.RoleState shape consumed by diff.RolesDiff.
func (l *LiveRoles) RoleState() *diff.RoleState {
	state := &diff.RoleState{
		RoleNames:    l.RoleNames,
		ManagedRoles: l.ManagedRoles,
		Grants:       make([]*diff.RoleACLGrant, 0, len(l.Grants)),
	}
	for _, key := range sortedGrantKeys(l.Grants) {
		g := l.Grants[key]
		privs := make([]string, 0, len(g.Privileges))
		for p := range g.Privileges {
			privs = append(privs, p)
		}
		slices.Sort(privs)
		options := make([]string, 0, len(g.GrantOption))
		for p := range g.GrantOption {
			if g.Privileges[p] {
				options = append(options, p)
			}
		}
		slices.Sort(options)
		state.Grants = append(state.Grants, &diff.RoleACLGrant{
			Grantee:      g.Grantee,
			ObjectKind:   g.ObjectKind,
			ObjectName:   g.ObjectName,
			Privileges:   privs,
			GrantOptions: options,
		})
	}
	return state
}

// sortedGrantKeys returns live grant keys in deterministic order.
func sortedGrantKeys(m map[string]*LiveGrant) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
