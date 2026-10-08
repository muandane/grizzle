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
	// RoleAttrs holds live login/limit/config attributes keyed by canonical
	// role identity.
	RoleAttrs map[string]*diff.LiveRoleAttrs
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

// InspectLiveRoles reads managed-role markers, role attributes, role-level
// settings, and object ACLs. Tables and sequences are scoped to the target
// schemas; schema ACLs cover the target schemas; the database ACL covers the
// current database. desiredPasswords maps canonical role identity -> plaintext
// password for server-side hash comparison (never returned to callers).
func InspectLiveRoles(ctx context.Context, dbtx dialect.DBTX, targetSchemas []string, desiredPasswords map[string]string) (*LiveRoles, error) {
	live := &LiveRoles{
		RoleNames:    make(map[string]string),
		ManagedRoles: make(map[string]bool),
		RoleAttrs:    make(map[string]*diff.LiveRoleAttrs),
		Grants:       make(map[string]*LiveGrant),
	}

	// 1. Roles: attributes, password hash, managed marker comment.
	// Role comments live in pg_shdescription (obj_description does not see
	// shared-object comments), keyed by the pg_authid catalog class.
	roleRows, err := dbtx.QueryContext(ctx, `
		SELECT r.rolname,
		       r.rolcanlogin,
		       r.rolconnlimit,
		       COALESCE(r.rolvaliduntil::text, ''),
		       r.rolinherit,
		       r.rolcreatedb,
		       r.rolcreaterole,
		       COALESCE(a.rolpassword, ''),
		       COALESCE((SELECT d.description
		                 FROM pg_shdescription d
		                 WHERE d.objoid = r.oid
		                   AND d.classoid = 'pg_authid'::regclass
		                ), '') AS comment
		FROM pg_roles r
		JOIN pg_authid a ON a.oid = r.oid
		WHERE r.rolname NOT LIKE 'pg\_%';
	`)
	if err != nil {
		return nil, fmt.Errorf("inspecting roles: %w", err)
	}
	if err := scanRows(roleRows, func(scan func(...any) error) error {
		var name, validUntil, passwordHash, comment string
		var canLogin, inherit, createDB, createRole bool
		var connLimit int
		if err := scan(&name, &canLogin, &connLimit, &validUntil, &inherit, &createDB, &createRole, &passwordHash, &comment); err != nil {
			return fmt.Errorf("scanning roles: %w", err)
		}
		key := schema.CanonicalIdentifierKey(name)
		live.RoleNames[key] = name
		if roleManagedComment(comment) {
			live.ManagedRoles[key] = true
		}
		attrs := &diff.LiveRoleAttrs{
			CanLogin:    canLogin,
			ConnLimit:   connLimit,
			ValidUntil:  normalizeValidUntil(validUntil),
			Inherit:     inherit,
			CreateDB:    createDB,
			CreateRole:  createRole,
			HasPassword: passwordHash != "",
			Config:      make(map[string]string),
		}
		if desired, ok := desiredPasswords[key]; ok && desired != "" {
			attrs.PasswordMatches = passwordMatchesStored(desired, passwordHash, name)
		} else {
			attrs.PasswordMatches = true
		}
		live.RoleAttrs[key] = attrs
		return nil
	}); err != nil {
		return nil, err
	}

	// 1b. Role-level settings (database-independent: setdatabase = 0).
	settingRows, err := dbtx.QueryContext(ctx, `
		SELECT r.rolname, u.entry
		FROM pg_db_role_setting s
		JOIN pg_roles r ON r.oid = s.setrole
		CROSS JOIN LATERAL unnest(s.setconfig) AS u(entry)
		WHERE s.setdatabase = 0
		  AND r.rolname NOT LIKE 'pg\_%';
	`)
	if err != nil {
		return nil, fmt.Errorf("inspecting role settings: %w", err)
	}
	if err := scanRows(settingRows, func(scan func(...any) error) error {
		var name, entry string
		if err := scan(&name, &entry); err != nil {
			return fmt.Errorf("scanning role settings: %w", err)
		}
		key := schema.CanonicalIdentifierKey(name)
		attrs := live.RoleAttrs[key]
		if attrs == nil {
			attrs = &diff.LiveRoleAttrs{Config: make(map[string]string)}
			live.RoleAttrs[key] = attrs
		}
		if attrs.Config == nil {
			attrs.Config = make(map[string]string)
		}
		param, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil
		}
		attrs.Config[strings.ToLower(param)] = value
		return nil
	}); err != nil {
		return nil, err
	}

	// 2. Table-like relation + sequence ACLs scoped to target schemas.
	// INNER join on aclexplode skips NULL relacl objects (no explicit grants
	// to manage). Views, materialized views, foreign tables, and partitioned
	// tables all accept the TABLE grant contract.
	schemaList, args := placeholders(targetSchemas)
	aclQuery := fmt.Sprintf(`
		SELECT n.nspname, c.relname, c.relkind,
		       a.grantee = 0 AS is_public, COALESCE(r.rolname, '') AS rolname,
		       a.privilege_type, a.is_grantable
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN aclexplode(c.relacl) a ON true
		LEFT JOIN pg_roles r ON r.oid = a.grantee
		WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
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

// ValidateFunctionGrantTargets rejects FUNCTION grants that cannot be
// represented by the inspected ACL contract. PostgreSQL distinguishes
// functions from procedures and aggregates even though all have pg_proc
// rows; only prokind='f' is valid for GRANT ... ON FUNCTION.
func ValidateFunctionGrantTargets(ctx context.Context, dbtx dialect.DBTX, spec *schema.RolesSpec, targetSchema string, shadowMap map[string]string) error {
	if spec == nil {
		return nil
	}
	for _, grant := range spec.Grants {
		if !strings.EqualFold(grant.ObjectKind, "FUNCTION") {
			continue
		}
		canonical := schema.CanonicalGrantObject("FUNCTION", grant.ObjectName, targetSchema)
		name, suffix, ok := splitFunctionObject(canonical)
		if !ok || !strings.HasSuffix(suffix, ")") {
			return fmt.Errorf("FUNCTION grant target %q has an invalid identity", grant.ObjectName)
		}
		parts := schema.ParseQualifiedIdentifier(name)
		if len(parts) != 2 {
			return fmt.Errorf("FUNCTION grant target %q must resolve to a schema-qualified function", grant.ObjectName)
		}
		argsText := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(suffix, "("), ")"))
		if len(argsText) == 0 && suffix != "()" {
			return fmt.Errorf("FUNCTION grant target %q has an invalid argument list", grant.ObjectName)
		}
		schemaName := decodeCatalogIdentifier(parts[0])
		functionName := decodeCatalogIdentifier(parts[1])
		candidateSchemas := []string{schemaName}
		if shadowSchema, ok := shadowSchemaForTarget(shadowMap, schemaName); ok {
			candidateSchemas = []string{shadowSchema, schemaName}
		}
		placeholders := make([]string, 0, len(candidateSchemas))
		args := make([]any, 0, len(candidateSchemas)+3)
		args = append(args, functionName, argsText)
		for i, candidateSchema := range candidateSchemas {
			placeholders = append(placeholders, fmt.Sprintf("$%d", i+3))
			args = append(args, candidateSchema)
		}
		orderCases := make([]string, 0, len(candidateSchemas))
		for i := range candidateSchemas {
			orderCases = append(orderCases, fmt.Sprintf("WHEN $%d THEN %d", i+3, i+1))
		}
		var prokind string
		err := dbtx.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT COALESCE((
				SELECT p.prokind
				FROM pg_proc p
				JOIN pg_namespace n ON n.oid = p.pronamespace
				WHERE p.proname = $1
				  AND pg_get_function_identity_arguments(p.oid) = $2
				  AND n.nspname IN (%s)
				  AND p.prokind = 'f'
				ORDER BY CASE n.nspname %s END
				LIMIT 1
			), '');`, strings.Join(placeholders, ", "), strings.Join(orderCases, " ")), args...).Scan(&prokind)
		if err != nil {
			return fmt.Errorf("checking FUNCTION grant target %q: %w", grant.ObjectName, err)
		}
		if prokind == "" {
			// Run a diagnostic lookup only after the ordinary-function lookup
			// has failed. This preserves shadow/search-path precedence for
			// valid functions while still reporting unsupported routine kinds
			// clearly when no valid function exists.
			err := dbtx.QueryRowContext(ctx, fmt.Sprintf(`
				SELECT COALESCE((
					SELECT p.prokind
					FROM pg_proc p
					JOIN pg_namespace n ON n.oid = p.pronamespace
					WHERE p.proname = $1
					  AND pg_get_function_identity_arguments(p.oid) = $2
					  AND n.nspname IN (%s)
					ORDER BY CASE n.nspname %s END
					LIMIT 1
				), '');`, strings.Join(placeholders, ", "), strings.Join(orderCases, " ")), args...).Scan(&prokind)
			if err != nil {
				return fmt.Errorf("checking FUNCTION grant target %q: %w", grant.ObjectName, err)
			}
		}
		if prokind == "" {
			return fmt.Errorf("FUNCTION grant target %q does not resolve to a managed routine", grant.ObjectName)
		}
		if prokind != "f" {
			return fmt.Errorf("FUNCTION grant target %q resolves to unsupported PostgreSQL routine kind %q; aggregates, procedures, and window functions are not supported", grant.ObjectName, prokind)
		}
	}
	return nil
}

func roleManagedComment(comment string) bool {
	return comment == schema.RoleManagedComment
}

// RoleHasManagedMarker verifies the exact catalog marker immediately before
// a DROP ROLE. Direct plan application cannot rely on the marker snapshot
// captured during planning.
func RoleHasManagedMarker(ctx context.Context, dbtx dialect.DBTX, roleName string) (bool, error) {
	var managed bool
	err := dbtx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_roles r
			JOIN pg_shdescription d
			  ON d.objoid = r.oid
			 AND d.classoid = 'pg_authid'::regclass
			WHERE r.rolname = $1
			  AND d.description = $2
		);`, roleName, schema.RoleManagedComment).Scan(&managed)
	if err != nil {
		return false, fmt.Errorf("checking managed marker for role %q: %w", roleName, err)
	}
	return managed, nil
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

// RoleOwnsObjects reports whether the named role owns any catalog object or
// dependency that makes DROP ROLE unsafe. The check is intentionally
// conservative: default ACLs, memberships, and ownership in catalogs not
// managed by Grizzle all refuse the drop.
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
			(SELECT count(*) FROM pg_publication WHERE pubowner = r.oid) +
			(SELECT count(*) FROM pg_default_acl  WHERE defaclrole = r.oid) +
			(SELECT count(*) FROM pg_auth_members WHERE roleid = r.oid OR member = r.oid) +
			(SELECT count(*) FROM pg_extension    WHERE extowner = r.oid) +
			(SELECT count(*) FROM pg_foreign_data_wrapper WHERE fdwowner = r.oid) +
			(SELECT count(*) FROM pg_foreign_server WHERE srvowner = r.oid) +
			(SELECT count(*) FROM pg_event_trigger WHERE evtowner = r.oid) +
			(SELECT count(*) FROM pg_subscription WHERE subowner = r.oid) +
			(SELECT count(*) FROM pg_tablespace WHERE spcowner = r.oid) +
			(SELECT count(*) FROM pg_collation WHERE collowner = r.oid) +
			(SELECT count(*) FROM pg_conversion WHERE conowner = r.oid) +
			(SELECT count(*) FROM pg_largeobject_metadata WHERE lomowner = r.oid)
		) > 0
		FROM pg_roles r WHERE r.rolname = $1;
	`, roleName).Scan(&owns)
	if err != nil {
		return false, fmt.Errorf("checking ownership for role %q: %w", roleName, err)
	}
	return owns, nil
}

// RoleHasUnhandledDependencies checks shared-catalog dependencies that are
// not ACL rows. ACL dependencies are handled separately so in-scope grants
// can be revoked before DROP ROLE; every other dependency fails closed.
func RoleHasUnhandledDependencies(ctx context.Context, dbtx dialect.DBTX, roleName string) (bool, error) {
	var hasDependency bool
	err := dbtx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_shdepend d
			JOIN pg_roles r
			  ON d.refclassid = 'pg_authid'::regclass
			 AND d.refobjid = r.oid
			WHERE r.rolname = $1
			  AND d.deptype <> 'a'
		);`, roleName).Scan(&hasDependency)
	if err != nil {
		return false, fmt.Errorf("checking shared dependencies for role %q: %w", roleName, err)
	}
	return hasDependency, nil
}

// RoleHasUnhandledACLs reports ACL entries that the RolesSQL inspector does
// not reconcile for a managed role. In-scope relation/function/schema/current
// database ACLs are deliberately excluded because RolesDiff emits revokes
// for those entries before DROP ROLE. Everything else is a conservative
// refusal rather than a DROP ROLE that may fail after schema steps.
func RoleHasUnhandledACLs(ctx context.Context, dbtx dialect.DBTX, roleName string, targetSchemas []string) (bool, error) {
	schemaArgs := make([]any, 0, len(targetSchemas))
	schemaPlaceholders := make([]string, 0, len(targetSchemas))
	for i, targetSchema := range targetSchemas {
		schemaPlaceholders = append(schemaPlaceholders, fmt.Sprintf("$%d", i+2))
		schemaArgs = append(schemaArgs, targetSchema)
	}
	schemaFilter := "TRUE"
	schemaInFilter := "FALSE"
	if len(schemaPlaceholders) > 0 {
		schemaFilter = "n.nspname NOT IN (" + strings.Join(schemaPlaceholders, ", ") + ")"
		schemaInFilter = "n.nspname IN (" + strings.Join(schemaPlaceholders, ", ") + ")"
	}
	query := fmt.Sprintf(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_roles r
			WHERE r.rolname = $1
			  AND (
				EXISTS (
					SELECT 1
					FROM pg_class c
					JOIN pg_namespace n ON n.oid = c.relnamespace
					CROSS JOIN LATERAL aclexplode(c.relacl) a
					WHERE a.grantee = r.oid
					  AND %s
				)
				OR EXISTS (
					SELECT 1
					FROM pg_proc p
					JOIN pg_namespace n ON n.oid = p.pronamespace
					CROSS JOIN LATERAL aclexplode(p.proacl) a
					WHERE a.grantee = r.oid
					  AND (%s OR p.prokind <> 'f')
				)
				OR EXISTS (
					SELECT 1
					FROM pg_namespace n
					CROSS JOIN LATERAL aclexplode(n.nspacl) a
					WHERE a.grantee = r.oid
					  AND %s
				)
				OR EXISTS (
					SELECT 1
					FROM pg_database d
					CROSS JOIN LATERAL aclexplode(d.datacl) a
					WHERE a.grantee = r.oid
					  AND d.datname <> current_database()
				)
				OR EXISTS (
					SELECT 1
					FROM pg_default_acl d
					CROSS JOIN LATERAL aclexplode(d.defaclacl) a
					WHERE a.grantee = r.oid
				)
				OR EXISTS (
					SELECT 1
					FROM pg_shdepend d
					WHERE d.refclassid = 'pg_authid'::regclass
					  AND d.refobjid = r.oid
					  AND d.deptype = 'a'
					  AND (
						d.classid NOT IN (
							'pg_class'::regclass,
							'pg_proc'::regclass,
							'pg_namespace'::regclass,
							'pg_database'::regclass
						)
						OR NOT (
							(d.classid = 'pg_class'::regclass AND EXISTS (
								SELECT 1
								FROM pg_class c
								JOIN pg_namespace n ON n.oid = c.relnamespace
								WHERE c.oid = d.objid
								  AND %s
								  AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
							))
							OR (d.classid = 'pg_proc'::regclass AND EXISTS (
								SELECT 1
								FROM pg_proc p
								JOIN pg_namespace n ON n.oid = p.pronamespace
								WHERE p.oid = d.objid
								  AND %s
								  AND p.prokind = 'f'
							))
							OR (d.classid = 'pg_namespace'::regclass AND EXISTS (
								SELECT 1
								FROM pg_namespace n
								WHERE n.oid = d.objid
								  AND %s
							))
							OR (d.classid = 'pg_database'::regclass AND EXISTS (
								SELECT 1
								FROM pg_database db
								WHERE db.oid = d.objid
								  AND db.datname = current_database()
							))
						)
					  )
				)
			)
		);`, schemaFilter, schemaFilter, schemaFilter,
		schemaInFilter, schemaInFilter, schemaInFilter)
	args := append([]any{roleName}, schemaArgs...)
	var hasACL bool
	if err := dbtx.QueryRowContext(ctx, query, args...).Scan(&hasACL); err != nil {
		return false, fmt.Errorf("checking unhandled ACLs for role %q: %w", roleName, err)
	}
	return hasACL, nil
}

func normalizeValidUntil(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "infinity") {
		return ""
	}
	return value
}

// GenerateCreateRoleSQL renders CREATE ROLE with declared attributes.
// When Login is unset or false the PostgreSQL default NOLOGIN is rendered.
// Password plaintext is never included; use GenerateAlterRolePasswordSQL at
// apply time when Role.HasPassword is set.
func GenerateCreateRoleSQL(role *schema.Role) string {
	if role == nil {
		return "CREATE ROLE ;"
	}
	var b strings.Builder
	b.WriteString("CREATE ROLE ")
	b.WriteString(quoteIdentifier(role.Name))
	attrs := renderRoleAttrClauses(role, false)
	if attrs != "" {
		b.WriteByte(' ')
		b.WriteString(attrs)
	} else {
		b.WriteString(" NOLOGIN")
	}
	b.WriteByte(';')
	return b.String()
}

// GenerateCreateRoleSQLRedacted renders CREATE ROLE for plan steps. Passwords
// are applied via a follow-up ALTER_ROLE step so CREATE SQL never embeds secrets.
func GenerateCreateRoleSQLRedacted(role *schema.Role) string {
	return GenerateCreateRoleSQL(role)
}

func renderRoleAttrClauses(role *schema.Role, forAlter bool) string {
	if role == nil {
		return ""
	}
	var parts []string
	if role.Login != nil {
		if *role.Login {
			parts = append(parts, "LOGIN")
		} else {
			parts = append(parts, "NOLOGIN")
		}
	} else if !forAlter {
		parts = append(parts, "NOLOGIN")
	}
	if role.Inherit != nil {
		if *role.Inherit {
			parts = append(parts, "INHERIT")
		} else {
			parts = append(parts, "NOINHERIT")
		}
	}
	if role.CreateDB != nil {
		if *role.CreateDB {
			parts = append(parts, "CREATEDB")
		} else {
			parts = append(parts, "NOCREATEDB")
		}
	}
	if role.CreateRole != nil {
		if *role.CreateRole {
			parts = append(parts, "CREATEROLE")
		} else {
			parts = append(parts, "NOCREATEROLE")
		}
	}
	if role.ConnectionLimit != nil {
		parts = append(parts, fmt.Sprintf("CONNECTION LIMIT %d", *role.ConnectionLimit))
	}
	if role.ValidUntil != "" {
		parts = append(parts, "VALID UNTIL "+quoteStringLiteral(role.ValidUntil))
	}
	return strings.Join(parts, " ")
}

// GenerateAlterRoleAttrsSQL renders ALTER ROLE ... WITH attr clauses (no password).
func GenerateAlterRoleAttrsSQL(role *schema.Role) string {
	attrs := renderRoleAttrClauses(role, true)
	if attrs == "" {
		return ""
	}
	return fmt.Sprintf("ALTER ROLE %s WITH %s;", quoteIdentifier(role.Name), attrs)
}

// GenerateAlterRoleSetSQL renders ALTER ROLE ... SET/RESET for config drift.
func GenerateAlterRoleSetSQL(roleName, param, value string, reset bool) string {
	ident := quoteIdentifier(roleName)
	if reset {
		return fmt.Sprintf("ALTER ROLE %s RESET %s;", ident, param)
	}
	if value == schema.ConfigFromCurrent {
		return fmt.Sprintf("ALTER ROLE %s SET %s FROM CURRENT;", ident, param)
	}
	return fmt.Sprintf("ALTER ROLE %s SET %s = %s;", ident, param, quoteConfigValue(value))
}

func quoteConfigValue(value string) string {
	// Numbers and simple keywords stay unquoted; everything else is a literal.
	if _, err := fmt.Sscanf(value, "%d", new(int)); err == nil && fmt.Sprintf("%d", atoiOrZero(value)) == value {
		return value
	}
	upper := strings.ToUpper(value)
	if upper == "ON" || upper == "OFF" || upper == "TRUE" || upper == "FALSE" {
		return value
	}
	return quoteStringLiteral(value)
}

func atoiOrZero(value string) int {
	var n int
	_, _ = fmt.Sscanf(value, "%d", &n)
	return n
}

// GenerateAlterRoleStepSQL renders redacted ALTER ROLE SQL for plan steps.
// Password plaintext is never included; apply rebuilds it from Role IR.
func GenerateAlterRoleStepSQL(role *schema.Role) string {
	if role == nil {
		return ""
	}
	var stmts []string
	if attrs := GenerateAlterRoleAttrsSQL(role); attrs != "" {
		stmts = append(stmts, attrs)
	}
	if role.HasPassword {
		stmts = append(stmts, GenerateAlterRolePasswordSQLRedacted(role.Name))
	}
	if len(role.Config) > 0 {
		keys := make([]string, 0, len(role.Config))
		for k := range role.Config {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, key := range keys {
			value := role.Config[key]
			stmts = append(stmts, GenerateAlterRoleSetSQL(role.Name, key, value, value == ""))
		}
	}
	return strings.Join(stmts, "\n")
}

// GenerateAlterRoleApplySQL rebuilds executable ALTER ROLE SQL including the
// real password from IR. Used only at apply time.
func GenerateAlterRoleApplySQL(role *schema.Role) string {
	if role == nil {
		return ""
	}
	var stmts []string
	if attrs := GenerateAlterRoleAttrsSQL(role); attrs != "" {
		stmts = append(stmts, attrs)
	}
	if role.HasPassword {
		stmts = append(stmts, GenerateAlterRolePasswordSQL(role.Name, role.Password))
	}
	if len(role.Config) > 0 {
		keys := make([]string, 0, len(role.Config))
		for k := range role.Config {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, key := range keys {
			value := role.Config[key]
			stmts = append(stmts, GenerateAlterRoleSetSQL(role.Name, key, value, value == ""))
		}
	}
	return strings.Join(stmts, "\n")
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
		RoleAttrs:    l.RoleAttrs,
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
