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
func diffCatalogSteps(ctx context.Context, dbtx dialect.DBTX, cfg PostgresExecConfig) ([]plan.Step, error) {
	desired, err := desiredCatalogSpec(cfg)
	if err != nil {
		return nil, err
	}
	if desired == nil {
		return nil, nil
	}

	live, err := postgres.InspectLiveCatalog(ctx, dbtx)
	if err != nil {
		return nil, fmt.Errorf("%w: catalog: %w", plan.ErrInspectionFailed, err)
	}

	// Event-trigger functions must exist: managed functions are created in
	// the shadow tx, so by this point their definitions are live inside it.
	// Creating a trigger whose function is missing would fail at apply with
	// a confusing error, so refuse up front with the offending trigger name.
	for _, e := range desired.EventTriggers {
		lookupSchemas := []string{cfg.ShadowSchema, cfg.primarySchema(), "public"}
		exists, err := postgres.EventTriggerFunctionExists(ctx, dbtx, e.Function, lookupSchemas...)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
		}
		if !exists {
			return nil, fmt.Errorf("event trigger %q references function %q which does not exist; manage the function in SchemaSQL or create it before syncing the catalog", e.Name, e.Function)
		}
	}

	changes := diff.CatalogDiff(desired, live.CatalogState(), cfg.primarySchema())

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
	if err := schema.ValidateCatalogSpecMerge(schemaSpec, sideSpec); err != nil {
		return nil, fmt.Errorf("validating catalog statement bases: %w", err)
	}
	return schema.MergeCatalogSpecs(schemaSpec, sideSpec), nil
}
