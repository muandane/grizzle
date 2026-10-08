package diff

import (
	"maps"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// RoleACLGrant is one live ACL entry (grantee, object) with its privileges.
type RoleACLGrant struct {
	Grantee      string   // role name or PUBLIC
	ObjectKind   string   // TABLE, SEQUENCE, DATABASE, SCHEMA, FUNCTION
	ObjectName   string   // canonical, schema-qualified where applicable
	Privileges   []string // sorted privilege_type values
	GrantOptions []string // subset of Privileges held WITH GRANT OPTION
}

// LiveRoleAttrs is the live attribute snapshot for one role.
type LiveRoleAttrs struct {
	CanLogin        bool
	ConnLimit       int
	ValidUntil      string
	Inherit         bool
	CreateDB        bool
	CreateRole      bool
	HasPassword     bool
	PasswordMatches bool // true when no desired password or hashes match
	Config          map[string]string
}

// RoleState is the live role/ACL snapshot consumed by RolesDiff. It is built
// by the dialect layer (postgres.InspectLiveRoles) so this package stays free
// of dialect imports.
type RoleState struct {
	// RoleNames maps canonical PostgreSQL role identity -> catalog name
	// (non-system roles).
	RoleNames map[string]string
	// ManagedRoles are canonical names of roles stamped with the
	// grizzle-managed marker comment.
	ManagedRoles map[string]bool
	// RoleAttrs maps canonical role identity -> live attributes.
	RoleAttrs map[string]*LiveRoleAttrs
	// Grants is the live ACL inventory.
	Grants []*RoleACLGrant
}

// RolesDiff compares the desired RolesSpec (parsed from RolesSQL) with live
// role/ACL state and returns pure changes.
//
// Scope rules:
//   - Roles are created for every desired declaration and for grantees
//     referenced by grants (NOLOGIN group roles), stamped with the
//     managed-role marker comment.
//   - Live roles carrying the marker that are absent from the desired spec
//     are dropped behind AllowDropRole; operator-created roles are never
//     swept.
//   - Grants are diffed per (object, grantee): missing privileges GRANT,
//     surplus privileges REVOKE (gated by AllowRevoke). Grants to grantees
//     outside the managed set are operator state and left untouched.
//   - ACL grants held by roles being dropped are revoked before DROP ROLE so
//     PostgreSQL can remove the role even when explicit ACL dependencies remain.
//   - Grant-option drift is reconciled by re-grant (option missing) or
//     revoke + re-grant (option unwanted).
//
// targetSchema qualifies unqualified TABLE/SEQUENCE object names so desired
// grants written against the working schema match the schema-qualified names
// reported by ACL introspection.
func RolesDiff(desired *schema.RolesSpec, live *RoleState, targetSchema string) []Change {
	var changes []Change
	if live == nil {
		live = &RoleState{}
	}

	desiredRoles := desiredRoleNames(desired)
	desiredRoleIR := indexDesiredRoles(desired)
	liveGrants := indexLiveGrants(live.Grants)
	droppedRoles := rolesBeingDropped(live.ManagedRoles, desiredRoles)

	// 1. Role creation: declared roles and grantees missing live.
	for _, roleName := range sortedRoleNames(desiredRoles) {
		if _, exists := live.RoleNames[roleName]; exists {
			continue
		}
		role := desiredRoleIR[roleName]
		if role == nil {
			role = &schema.Role{Name: roleName}
		}
		changes = append(changes,
			Change{
				Type:  plan.ChangeCreateRole,
				Table: roleName,
				Role:  schema.CloneRole(role),
			},
		)
		if role.HasPassword {
			// Password on create is applied as a follow-up ALTER so Step.SQL
			// for CREATE stays free of plaintext and apply can rebuild it.
			changes = append(changes, Change{
				Type:  plan.ChangeAlterRole,
				Table: roleName,
				Role: &schema.Role{
					Name:        role.Name,
					Password:    role.Password,
					HasPassword: true,
				},
			})
		}
		if len(role.Config) > 0 {
			changes = append(changes, Change{
				Type:  plan.ChangeAlterRole,
				Table: roleName,
				Role:  &schema.Role{Name: role.Name, Config: copyStringMap(role.Config)},
			})
		}
		changes = append(changes, Change{
			Type:  plan.ChangeRoleComment,
			Table: roleName,
			Role:  &schema.Role{Name: roleName},
		})
	}

	// 1b. Attribute / password / config drift for roles that already exist.
	for _, roleName := range sortedRoleNames(desiredRoles) {
		if _, exists := live.RoleNames[roleName]; !exists {
			continue
		}
		desiredRole := desiredRoleIR[roleName]
		if desiredRole == nil {
			continue
		}
		liveAttrs := live.RoleAttrs[roleName]
		changes = append(changes, roleAttrDriftChanges(desiredRole, liveAttrs)...)
	}

	// 2. Revoke ACLs held by managed roles that are about to be dropped.
	// DROP ROLE is gated separately; these revokes remain gated by
	// AllowRevoke and sort before DROP_ROLE.
	for _, key := range sortedKeys(liveGrants) {
		lg := liveGrants[key]
		grantee := schema.CanonicalRoleName(lg.Grantee)
		if !droppedRoles[grantee] || schema.IsPublicRoleIdentifier(lg.Grantee) {
			continue
		}
		r := &schema.Grant{
			Grantee:    lg.Grantee,
			ObjectKind: lg.ObjectKind,
			ObjectName: lg.ObjectName,
			Privileges: slices.Clone(lg.Privileges),
		}
		changes = append(changes, grantChange(plan.ChangeRevoke, r))
	}

	// 3. Role drops: marker present live, absent from desired.
	for _, roleName := range sortedRoleNames(live.ManagedRoles) {
		if desiredRoles[roleName] {
			continue
		}
		changes = append(changes, Change{
			Type:        plan.ChangeDropRole,
			Table:       roleName,
			Role:        &schema.Role{Name: roleName},
			Destructive: true,
		})
	}

	// 4. Desired grants: create missing, amend drifted entries.
	desiredGrants := indexDesiredGrants(desired, targetSchema)
	for _, key := range sortedKeys(desiredGrants) {
		dg := desiredGrants[key]
		lg := liveGrants[key]

		if lg == nil {
			changes = append(changes, grantChange(plan.ChangeGrant, dg))
			continue
		}
		missing := missingPrivileges(dg.Privileges, lg.Privileges)
		if schema.IsPublicRoleIdentifier(dg.Grantee) || schema.IsPublicRoleIdentifier(lg.Grantee) {
			if len(missing) > 0 {
				g := *dg
				g.Privileges = missing
				g.GrantOption = dg.GrantOption
				changes = append(changes, grantChange(plan.ChangeGrant, &g))
			}
			if missingOptions := subtractPrivileges(
				missingPrivileges(optionSubset(dg), lg.GrantOptions),
				missing,
			); len(missingOptions) > 0 {
				g := *dg
				g.Privileges = missingOptions
				g.GrantOption = true
				changes = append(changes, grantChange(plan.ChangeGrant, &g))
			}
			continue
		}
		missingOptions := missingPrivileges(optionSubset(dg), lg.GrantOptions)
		if len(missing) > 0 {
			g := *dg
			g.Privileges = missing
			// A privilege that is absent entirely can be granted with its
			// desired option in one statement. Emitting a plain GRANT first
			// creates redundant overlap and complicates approval review.
			g.GrantOption = dg.GrantOption
			changes = append(changes, grantChange(plan.ChangeGrant, &g))
		}
		if len(missingOptions) > 0 {
			missingOptions = subtractPrivileges(missingOptions, missing)
		}
		if len(missingOptions) > 0 {
			// Re-grant with option: GRANT ... WITH GRANT OPTION is additive
			// for privileges already held.
			g := *dg
			g.Privileges = missingOptions
			g.GrantOption = true
			changes = append(changes, grantChange(plan.ChangeGrant, &g))
		}
		if unwantedOptions := missingPrivileges(lg.GrantOptions, optionSubset(dg)); len(unwantedOptions) > 0 {
			// Option held live but not desired: revoke the delegation only
			// (REVOKE GRANT OPTION FOR keeps the base privilege).
			r := *dg
			r.Privileges = unwantedOptions
			r.GrantOption = true
			changes = append(changes, grantChange(plan.ChangeRevoke, &r))
		}
		if surplus := surplusPrivileges(dg, lg); len(surplus) > 0 {
			r := *dg
			r.Privileges = surplus
			r.GrantOption = false
			changes = append(changes, grantChange(plan.ChangeRevoke, &r))
		}
	}

	// 5. Live-only grants: revoke behind the gate when the grantee is
	// managed; operator grantees are left untouched.
	for _, key := range sortedKeys(liveGrants) {
		if _, inDesired := desiredGrants[key]; inDesired {
			continue
		}
		lg := liveGrants[key]
		grantee := schema.CanonicalRoleName(lg.Grantee)
		if schema.IsPublicRoleIdentifier(lg.Grantee) {
			// PUBLIC grants outside the desired spec are operator state:
			// revoking them would break ambient access.
			continue
		}
		if !desiredRoles[grantee] && !live.ManagedRoles[grantee] {
			continue
		}
		if droppedRoles[grantee] {
			continue
		}
		r := &schema.Grant{
			Grantee:    lg.Grantee,
			ObjectKind: lg.ObjectKind,
			ObjectName: lg.ObjectName,
			Privileges: slices.Clone(lg.Privileges),
		}
		changes = append(changes, grantChange(plan.ChangeRevoke, r))
	}

	return changes
}

// grantChange wraps a grant IR into a Change.
func grantChange(typ plan.ChangeType, g *schema.Grant) Change {
	return Change{
		Type:        typ,
		Table:       g.ObjectName,
		Grant:       g,
		Destructive: typ == plan.ChangeRevoke,
	}
}

// desiredRoleNames unions declared roles and all grantees referenced by
// desired grants (PUBLIC excluded — it always exists).
func desiredRoleNames(spec *schema.RolesSpec) map[string]bool {
	names := make(map[string]bool)
	for _, r := range spec.Roles {
		names[schema.CanonicalIdentifierKey(r.Name)] = true
	}
	for _, g := range spec.Grants {
		if schema.IsPublicRoleIdentifier(g.Grantee) {
			continue
		}
		names[schema.CanonicalRoleName(g.Grantee)] = true
	}
	return names
}

// rolesBeingDropped computes the set of live managed roles absent from desired.
func rolesBeingDropped(managed, desired map[string]bool) map[string]bool {
	dropped := make(map[string]bool)
	for name := range managed {
		if !desired[name] {
			dropped[name] = true
		}
	}
	return dropped
}

// indexDesiredGrants merges desired grants per (kind, object, grantee) key,
// qualifying unqualified table/sequence names with the target schema.
func indexDesiredGrants(spec *schema.RolesSpec, targetSchema string) map[string]*schema.Grant {
	out := make(map[string]*schema.Grant)
	for _, g := range spec.Grants {
		canonical := canonicalObjectName(g, targetSchema)
		key := schema.GrantKey(g.ObjectKind, canonical, g.Grantee)
		existing, ok := out[key]
		if !ok {
			out[key] = &schema.Grant{
				Grantee:            g.Grantee,
				ObjectKind:         g.ObjectKind,
				ObjectName:         g.ObjectName,
				Privileges:         slices.Clone(g.Privileges),
				GrantOption:        g.GrantOption,
				ExplicitPrivileges: slices.Clone(g.ExplicitPrivileges),
			}
			continue
		}
		for _, p := range g.Privileges {
			if !slices.Contains(existing.Privileges, p) {
				existing.Privileges = append(existing.Privileges, p)
			}
		}
		for _, privilege := range g.ExplicitPrivileges {
			if !slices.Contains(existing.ExplicitPrivileges, privilege) {
				existing.ExplicitPrivileges = append(existing.ExplicitPrivileges, privilege)
			}
		}
		existing.GrantOption = existing.GrantOption || g.GrantOption
	}
	return out
}

// canonicalObjectName normalizes PostgreSQL identifier identity and qualifies
// unqualified ACL objects against the target schema.
func canonicalObjectName(g *schema.Grant, targetSchema string) string {
	return schema.CanonicalGrantObject(g.ObjectKind, g.ObjectName, targetSchema)
}

// indexLiveGrants keys live ACL entries by (kind, object, grantee).
func indexLiveGrants(grants []*RoleACLGrant) map[string]*RoleACLGrant {
	out := make(map[string]*RoleACLGrant, len(grants))
	for _, g := range grants {
		key := schema.GrantKey(g.ObjectKind, g.ObjectName, g.Grantee)
		existing, ok := out[key]
		if !ok {
			out[key] = g
			continue
		}
		// Merge duplicate rows for the same key (paranoia; catalog folds them).
		for _, p := range g.Privileges {
			if !slices.Contains(existing.Privileges, p) {
				existing.Privileges = append(existing.Privileges, p)
			}
		}
		for _, p := range g.GrantOptions {
			if !slices.Contains(existing.GrantOptions, p) {
				existing.GrantOptions = append(existing.GrantOptions, p)
			}
		}
		slices.Sort(existing.Privileges)
		slices.Sort(existing.GrantOptions)
	}
	return out
}

// missingPrivileges returns desired privileges absent from the live list.
func missingPrivileges(desired []string, live []string) []string {
	var out []string
	for _, p := range desired {
		if !slices.Contains(live, p) {
			out = append(out, p)
		}
	}
	return out
}

func subtractPrivileges(values, remove []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !slices.Contains(remove, value) {
			out = append(out, value)
		}
	}
	return out
}

// optionSubset returns the privileges a desired grant holds WITH GRANT OPTION.
func optionSubset(g *schema.Grant) []string {
	if !g.GrantOption {
		return nil
	}
	return g.Privileges
}

// surplusPrivileges returns live privileges absent from desired.
func surplusPrivileges(dg *schema.Grant, lg *RoleACLGrant) []string {
	var out []string
	for _, p := range lg.Privileges {
		if !slices.Contains(dg.Privileges, p) {
			out = append(out, p)
		}
	}
	return out
}

func sortedRoleNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func indexDesiredRoles(spec *schema.RolesSpec) map[string]*schema.Role {
	out := make(map[string]*schema.Role)
	if spec == nil {
		return out
	}
	for _, role := range spec.Roles {
		out[schema.CanonicalIdentifierKey(role.Name)] = role
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func roleAttrDriftChanges(desired *schema.Role, live *LiveRoleAttrs) []Change {
	var changes []Change
	if desired == nil {
		return nil
	}
	attrDrift := &schema.Role{Name: desired.Name}
	hasAttrDrift := false
	if desired.Login != nil {
		liveLogin := live != nil && live.CanLogin
		if *desired.Login != liveLogin {
			v := *desired.Login
			attrDrift.Login = &v
			hasAttrDrift = true
		}
	}
	if desired.Inherit != nil {
		liveInherit := live == nil || live.Inherit
		if *desired.Inherit != liveInherit {
			v := *desired.Inherit
			attrDrift.Inherit = &v
			hasAttrDrift = true
		}
	}
	if desired.CreateDB != nil {
		liveCreateDB := live != nil && live.CreateDB
		if *desired.CreateDB != liveCreateDB {
			v := *desired.CreateDB
			attrDrift.CreateDB = &v
			hasAttrDrift = true
		}
	}
	if desired.CreateRole != nil {
		liveCreateRole := live != nil && live.CreateRole
		if *desired.CreateRole != liveCreateRole {
			v := *desired.CreateRole
			attrDrift.CreateRole = &v
			hasAttrDrift = true
		}
	}
	if desired.ConnectionLimit != nil {
		liveLimit := -1
		if live != nil {
			liveLimit = live.ConnLimit
		}
		if *desired.ConnectionLimit != liveLimit {
			v := *desired.ConnectionLimit
			attrDrift.ConnectionLimit = &v
			hasAttrDrift = true
		}
	}
	if desired.ValidUntil != "" {
		liveUntil := ""
		if live != nil {
			liveUntil = live.ValidUntil
		}
		if !validUntilEqual(desired.ValidUntil, liveUntil) {
			attrDrift.ValidUntil = desired.ValidUntil
			hasAttrDrift = true
		}
	}
	roleKey := schema.CanonicalIdentifierKey(desired.Name)
	if hasAttrDrift {
		changes = append(changes, Change{
			Type:  plan.ChangeAlterRole,
			Table: roleKey,
			Role:  attrDrift,
		})
	}
	if desired.HasPassword && (live == nil || !live.PasswordMatches) {
		changes = append(changes, Change{
			Type:  plan.ChangeAlterRole,
			Table: roleKey,
			Role: &schema.Role{
				Name:        desired.Name,
				Password:    desired.Password,
				HasPassword: true,
			},
		})
	}

	liveConfig := map[string]string{}
	if live != nil && live.Config != nil {
		liveConfig = live.Config
	}
	desiredConfig := desired.Config
	if desiredConfig == nil {
		desiredConfig = map[string]string{}
	}
	configRole := &schema.Role{Name: desired.Name, Config: make(map[string]string)}
	for _, key := range sortedKeys(desiredConfig) {
		desiredVal := desiredConfig[key]
		liveVal, liveOK := liveConfig[key]
		if desiredVal == schema.ConfigFromCurrent {
			// FROM CURRENT is a set-time directive; once the key exists live
			// we treat it as satisfied so second sync stays a no-op.
			if !liveOK {
				configRole.Config[key] = desiredVal
			}
			continue
		}
		if !liveOK || liveVal != desiredVal {
			configRole.Config[key] = desiredVal
		}
	}
	for _, key := range sortedKeys(liveConfig) {
		if _, ok := desiredConfig[key]; !ok {
			// Empty value is the RESET sentinel for renderers.
			configRole.Config[key] = ""
		}
	}
	if len(configRole.Config) > 0 {
		changes = append(changes, Change{
			Type:  plan.ChangeAlterRole,
			Table: roleKey,
			Role:  configRole,
		})
	}
	return changes
}

func validUntilEqual(desired, live string) bool {
	desired = strings.TrimSpace(desired)
	live = strings.TrimSpace(live)
	if desired == live {
		return true
	}
	// Live timestamps from PostgreSQL often include time; desired may be date-only.
	return strings.HasPrefix(live, desired)
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
