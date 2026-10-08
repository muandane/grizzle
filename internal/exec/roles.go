package exec

import (
	"context"
	"fmt"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// diffRolesSteps computes roles/grants steps from unified SchemaSQL role
// statements plus the optional RolesSQL side-channel. Unlike schema DDL,
// roles are never shadow-compiled: the desired state is statement-scanned,
// the live state is inspected from pg_roles/pg_authid and object ACLs, and
// the diff renders GRANT/REVOKE/CREATE ROLE/DROP ROLE steps that sort after
// all schema DDL.
func diffRolesSteps(ctx context.Context, dbtx dialect.DBTX, cfg PostgresExecConfig, serverVersion int) ([]plan.Step, error) {
	desired, err := desiredRolesSpec(cfg)
	if err != nil {
		return nil, err
	}
	if desired == nil {
		return nil, nil
	}
	var currentDatabase string
	if err := dbtx.QueryRowContext(ctx, `SELECT current_database();`).Scan(&currentDatabase); err != nil {
		return nil, fmt.Errorf("%w: current database: %w", plan.ErrInspectionFailed, err)
	}
	if err := schema.ValidateRolesSpecScope(desired, cfg.targetSchemas(), currentDatabase); err != nil {
		return nil, fmt.Errorf("validating role grant scope: %w", err)
	}
	_, shadowMap := eventTriggerFunctionLookup(cfg)
	if err := postgres.ValidateFunctionGrantTargets(ctx, dbtx, desired, cfg.primarySchema(), shadowMap); err != nil {
		return nil, fmt.Errorf("validating function grant targets: %w", err)
	}
	desired, err = schema.FilterRolePrivilegesForServer(desired, serverVersion)
	if err != nil {
		return nil, fmt.Errorf("validating role privileges: %w", err)
	}

	live, err := postgres.InspectLiveRoles(ctx, dbtx, cfg.targetSchemas())
	if err != nil {
		return nil, fmt.Errorf("%w: roles: %w", plan.ErrInspectionFailed, err)
	}

	changes := diff.RolesDiff(desired, live.RoleState(), cfg.primarySchema())

	steps := make([]plan.Step, 0, len(changes))
	for _, c := range changes {
		steps = append(steps, postgres.RenderChangeWithOpts(cfg.primarySchema(), c, postgres.RenderOpts{}))
	}
	var droppedRoles []string
	for _, step := range steps {
		if step.Type == plan.ChangeDropRole {
			droppedRoles = append(droppedRoles, step.Table)
		}
	}
	if err := validateRoleDropSafety(ctx, dbtx, droppedRoles, cfg.targetSchemas(), steps); err != nil {
		return nil, err
	}
	// Role steps carry their own priority band (119+, after all schema DDL);
	// sort them among themselves and rely on concatenation for the global
	// order, since the schema step list is already sorted.
	plan.SortSteps(steps)
	return steps, nil
}

func validateRoleDropSafety(ctx context.Context, dbtx dialect.DBTX, roleNames, targetSchemas []string, steps []plan.Step) error {
	var live *postgres.LiveRoles
	if len(roleNames) > 0 {
		var err error
		live, err = postgres.InspectLiveRoles(ctx, dbtx, targetSchemas)
		if err != nil {
			return fmt.Errorf("%w: roles: %w", plan.ErrInspectionFailed, err)
		}
	}
	for _, roleName := range roleNames {
		managed, err := postgres.RoleHasManagedMarker(ctx, dbtx, roleName)
		if err != nil {
			return fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
		}
		if !managed {
			return fmt.Errorf("refusing to drop role %q: exact grizzle-managed marker is absent", roleName)
		}
		owns, err := postgres.RoleOwnsObjects(ctx, dbtx, roleName)
		if err != nil {
			return fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
		}
		if owns {
			return fmt.Errorf("role %q has catalog ownership or dependency; transfer ownership or remove the role from the desired state before syncing roles", roleName)
		}
		dependency, err := postgres.RoleHasUnhandledDependencies(ctx, dbtx, roleName)
		if err != nil {
			return fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
		}
		if dependency {
			return fmt.Errorf("role %q has an unhandled shared-catalog dependency; transfer or remove it before dropping the role", roleName)
		}
		unhandledACL, err := postgres.RoleHasUnhandledACLs(ctx, dbtx, roleName, targetSchemas)
		if err != nil {
			return fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
		}
		if unhandledACL {
			return fmt.Errorf("role %q has ACL dependencies outside the managed role scope; revoke or transfer them before dropping the role", roleName)
		}
		if err := validatePlannedRoleACLRevokes(roleName, live.RoleState(), targetSchemas, steps); err != nil {
			return err
		}
	}
	return nil
}

func validatePlannedRoleACLRevokes(roleName string, live *diff.RoleState, targetSchemas []string, steps []plan.Step) error {
	planned := make(map[string]map[string]bool)
	for _, step := range steps {
		revoke, ok := parseRevokeStep(step)
		if !ok || revoke.grantOption ||
			schema.CanonicalRoleName(revoke.grantee) != schema.CanonicalRoleName(roleName) {
			continue
		}
		key := schema.GrantKey(
			revoke.objectKind,
			schema.CanonicalGrantObject(revoke.objectKind, revoke.objectName, targetSchemas[0]),
			roleName,
		)
		if planned[key] == nil {
			planned[key] = make(map[string]bool)
		}
		for _, privilege := range revoke.privileges {
			if privilege == "ALL PRIVILEGES" {
				planned[key]["*"] = true
				continue
			}
			planned[key][strings.ToUpper(privilege)] = true
		}
	}
	for _, grant := range live.Grants {
		if schema.IsPublicRoleIdentifier(grant.Grantee) ||
			schema.CanonicalRoleName(grant.Grantee) != schema.CanonicalRoleName(roleName) {
			continue
		}
		key := schema.GrantKey(
			grant.ObjectKind,
			schema.CanonicalGrantObject(grant.ObjectKind, grant.ObjectName, targetSchemas[0]),
			roleName,
		)
		covered := planned[key]
		for _, privilege := range grant.Privileges {
			if !covered["*"] && !covered[strings.ToUpper(privilege)] {
				return fmt.Errorf("refusing to drop role %q: plan does not revoke ACL privilege %s on %s", roleName, privilege, grant.ObjectName)
			}
		}
	}
	return nil
}

type parsedRevoke struct {
	objectKind  string
	objectName  string
	grantee     string
	privileges  []string
	grantOption bool
}

func parseRevokeStep(step plan.Step) (parsedRevoke, bool) {
	if step.Type != plan.ChangeRevoke {
		return parsedRevoke{}, false
	}
	sqlText := strings.TrimSpace(strings.TrimSuffix(step.SQL, ";"))
	upper := strings.ToUpper(sqlText)
	const prefix = "REVOKE "
	if !strings.HasPrefix(upper, prefix) {
		return parsedRevoke{}, false
	}
	body := strings.TrimSpace(sqlText[len(prefix):])
	grantOption := false
	const optionPrefix = "GRANT OPTION FOR "
	if strings.HasPrefix(strings.ToUpper(body), optionPrefix) {
		grantOption = true
		body = strings.TrimSpace(body[len(optionPrefix):])
	}
	onIndex := findSQLKeyword(body, " ON ")
	if onIndex < 0 {
		return parsedRevoke{}, false
	}
	fromIndex := findSQLKeyword(body[onIndex+len(" ON "):], " FROM ")
	if fromIndex < 0 {
		return parsedRevoke{}, false
	}
	fromIndex += onIndex + len(" ON ")
	privilegeText := strings.TrimSpace(body[:onIndex])
	objectText := strings.TrimSpace(body[onIndex+len(" ON ") : fromIndex])
	grantee := strings.TrimSpace(body[fromIndex+len(" FROM "):])
	objectKind, objectName, ok := splitObjectKind(objectText)
	if !ok || grantee == "" {
		return parsedRevoke{}, false
	}
	privileges := make([]string, 0)
	for _, privilege := range strings.Split(privilegeText, ",") {
		privilege = strings.ToUpper(strings.TrimSpace(privilege))
		if privilege != "" {
			privileges = append(privileges, privilege)
		}
	}
	if len(privileges) == 0 {
		return parsedRevoke{}, false
	}
	return parsedRevoke{
		objectKind:  objectKind,
		objectName:  objectName,
		grantee:     grantee,
		privileges:  privileges,
		grantOption: grantOption,
	}, true
}

func splitObjectKind(value string) (string, string, bool) {
	for i := 0; i < len(value); i++ {
		if value[i] == ' ' || value[i] == '\t' || value[i] == '\n' || value[i] == '\r' {
			kind := strings.TrimSpace(value[:i])
			name := strings.TrimSpace(value[i:])
			return strings.ToUpper(kind), name, kind != "" && name != ""
		}
	}
	return "", "", false
}

func findSQLKeyword(value, keyword string) int {
	inQuote := false
	parenDepth := 0
	for i := 0; i+len(keyword) <= len(value); i++ {
		switch value[i] {
		case '"':
			if inQuote && i+1 < len(value) && value[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
		case '(':
			if !inQuote {
				parenDepth++
			}
		case ')':
			if !inQuote && parenDepth > 0 {
				parenDepth--
			}
		}
		if !inQuote && parenDepth == 0 &&
			strings.EqualFold(value[i:i+len(keyword)], keyword) {
			return i
		}
	}
	return -1
}

func desiredRolesSpec(cfg PostgresExecConfig) (*schema.RolesSpec, error) {
	groups, err := splitSchemaSQL(cfg.SchemaSQL)
	if err != nil {
		return nil, err
	}
	if err := schema.ValidateRolesSQL(cfg.RolesSQL); err != nil {
		return nil, fmt.Errorf("validating RolesSQL: %w", err)
	}
	if strings.TrimSpace(groups.RolesSQL) == "" && strings.TrimSpace(cfg.RolesSQL) == "" {
		return nil, nil
	}
	return schema.MergeRolesSpecsForTarget(
		schema.ParseRolesSQL(groups.RolesSQL),
		schema.ParseRolesSQL(cfg.RolesSQL),
		cfg.primarySchema(),
	), nil
}
