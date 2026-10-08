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

	// Ownership refusal: a managed role that owns cluster objects must not be
	// dropped — the dependency transfer is an operator decision.
	for _, c := range changes {
		if c.Type != plan.ChangeDropRole {
			continue
		}
		owns, err := postgres.RoleOwnsObjects(ctx, dbtx, c.Table)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
		}
		if owns {
			return nil, fmt.Errorf("role %q has catalog ownership or dependency; transfer ownership or remove the role from the desired state before syncing roles", c.Table)
		}
		unhandledACL, err := postgres.RoleHasUnhandledACLs(ctx, dbtx, c.Table, cfg.targetSchemas())
		if err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
		}
		if unhandledACL {
			return nil, fmt.Errorf("role %q has ACL dependencies outside the managed role scope; revoke or transfer them before dropping the role", c.Table)
		}
	}

	steps := make([]plan.Step, 0, len(changes))
	for _, c := range changes {
		steps = append(steps, postgres.RenderChangeWithOpts(cfg.primarySchema(), c, postgres.RenderOpts{}))
	}
	// Role steps carry their own priority band (119+, after all schema DDL);
	// sort them among themselves and rely on concatenation for the global
	// order, since the schema step list is already sorted.
	plan.SortSteps(steps)
	return steps, nil
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
