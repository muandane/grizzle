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
func diffRolesSteps(ctx context.Context, dbtx dialect.DBTX, cfg PostgresExecConfig) ([]plan.Step, error) {
	desired, err := desiredRolesSpec(cfg)
	if err != nil {
		return nil, err
	}
	if desired == nil {
		return nil, nil
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
			return nil, fmt.Errorf("role %q owns objects; transfer ownership or remove the role from the desired state before syncing roles", c.Table)
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
	return schema.MergeRolesSpecs(
		schema.ParseRolesSQL(groups.RolesSQL),
		schema.ParseRolesSQL(cfg.RolesSQL),
	), nil
}
