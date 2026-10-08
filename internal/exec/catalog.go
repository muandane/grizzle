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

// diffCatalogSteps computes publication/event-trigger steps from unified
// SchemaSQL catalog statements plus the optional CatalogSQL side-channel.
// Catalog objects are never shadow-compiled: the desired state is
// statement-scanned, the live state is inspected from
// pg_publication/pg_event_trigger, and the diff renders CREATE/ALTER/DROP
// steps that sort after roles and grants.
func diffCatalogSteps(ctx context.Context, dbtx dialect.DBTX, cfg PostgresExecConfig, serverVersion int) ([]plan.Step, error) {
	desired, err := desiredCatalogSpec(cfg)
	if err != nil {
		return nil, err
	}
	if desired == nil {
		return nil, nil
	}
	if err := validateCatalogServerVersion(desired, serverVersion); err != nil {
		return nil, err
	}

	live, err := postgres.InspectLiveCatalog(ctx, dbtx)
	if err != nil {
		return nil, fmt.Errorf("%w: catalog: %w", plan.ErrInspectionFailed, err)
	}

	// Event-trigger functions must exist: managed functions are created in
	// the shadow tx, so by this point their definitions are live inside it.
	// Creating a trigger whose function is missing would fail at apply with
	// a confusing error, so refuse up front with the offending trigger name.
	lookupSchemas, shadowMap := eventTriggerFunctionLookup(cfg)
	for _, e := range desired.EventTriggers {
		exists, err := postgres.EventTriggerFunctionExistsWithShadowMap(ctx, dbtx, e.Function, shadowMap, lookupSchemas...)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
		}
		if !exists {
			return nil, fmt.Errorf("event trigger %q references function %q which does not exist; manage the function in SchemaSQL or create it before syncing the catalog", e.Name, e.Function)
		}
	}

	liveState := live.CatalogState()
	if err := refuseActiveSlotDrops(desired, liveState); err != nil {
		return nil, err
	}

	changes := diff.CatalogDiff(desired, liveState, cfg.primarySchema())

	steps := make([]plan.Step, 0, len(changes))
	for _, c := range changes {
		steps = append(steps, postgres.RenderChangeWithOpts(cfg.primarySchema(), c, postgres.RenderOpts{}))
	}
	// Catalog steps carry their own priority band (130+, after roles and
	// grants); sort them among themselves and rely on concatenation for the
	// global order, since the schema step list is already sorted.
	plan.SortSteps(steps)
	return steps, nil
}

// refuseActiveSlotDrops fails closed when an explicit slot drop targets a
// slot whose active_pid is non-NULL.
func refuseActiveSlotDrops(desired *schema.CatalogSpec, live *diff.CatalogLiveState) error {
	if desired == nil || live == nil {
		return nil
	}
	for name, slot := range live.ReplicationSlots {
		if slot == nil || slot.ActivePID == nil {
			continue
		}
		if desired.ReplicationSlotExplicitlyDropped(name) {
			return fmt.Errorf("refusing to drop logical replication slot %q: active_pid=%d (slot is in use)", name, *slot.ActivePID)
		}
	}
	return nil
}

func validateCatalogServerVersion(spec *schema.CatalogSpec, serverVersion int) error {
	if spec == nil || serverVersion == 0 || serverVersion >= 150000 {
		return nil
	}
	for _, publication := range spec.Publications {
		if len(publication.Schemas) > 0 {
			return fmt.Errorf("%w: publication %q uses FOR TABLES IN SCHEMA, which requires PostgreSQL 15 or newer", plan.ErrInvalidOptions, publication.Name)
		}
	}
	return nil
}

func desiredCatalogSpec(cfg PostgresExecConfig) (*schema.CatalogSpec, error) {
	groups, err := splitSchemaSQL(cfg.SchemaSQL)
	if err != nil {
		return nil, err
	}
	if err := schema.ValidateCatalogSQL(cfg.CatalogSQL); err != nil {
		return nil, fmt.Errorf("validating CatalogSQL: %w", err)
	}
	if strings.TrimSpace(groups.CatalogSQL) == "" && strings.TrimSpace(cfg.CatalogSQL) == "" {
		return nil, nil
	}
	schemaSpec := schema.ParseCatalogSQL(groups.CatalogSQL)
	sideSpec := schema.ParseCatalogSQL(cfg.CatalogSQL)
	if err := schema.ValidateCatalogSpecMergeForTarget(schemaSpec, sideSpec, cfg.primarySchema()); err != nil {
		return nil, fmt.Errorf("validating catalog statement bases: %w", err)
	}
	return schema.MergeCatalogSpecsForTarget(schemaSpec, sideSpec, cfg.primarySchema()), nil
}

// eventTriggerFunctionLookup returns every shadow and target namespace that
// can contain a routine compiled by the schema diff. Qualified functions are
// rewritten to their mapped shadow schema; unqualified functions search the
// same ordered namespace set as multi-schema shadow compilation.
func eventTriggerFunctionLookup(cfg PostgresExecConfig) ([]string, map[string]string) {
	targetSchemas := cfg.targetSchemas()
	shadowMap := make(map[string]string)
	lookup := make([]string, 0, len(targetSchemas)*2+1)
	if len(targetSchemas) == 1 {
		shadowSchema := cfg.ShadowSchema
		if shadowSchema == "" {
			shadowSchema = "_grizzle_shadow"
		}
		shadowMap[targetSchemas[0]] = shadowSchema
		lookup = append(lookup, shadowSchema)
	} else {
		shadowMap = postgres.ComputeShadowSchemas(cfg.ShadowSchema, targetSchemas)
		for _, targetSchema := range targetSchemas {
			lookup = append(lookup, shadowMap[targetSchema])
			lookup = append(lookup, targetSchema)
		}
	}
	if len(targetSchemas) == 1 {
		lookup = append(lookup, targetSchemas[0])
	}
	lookup = append(lookup, "public")
	return lookup, shadowMap
}
