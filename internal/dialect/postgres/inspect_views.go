package postgres

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/schema"
)

// inspectViews reads views and materialized views, and queries pg_depend for view dependencies.
func inspectViews(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	viewQuery := `
		SELECT
			c.relname AS view_name,
			c.relkind AS view_kind,
			pg_get_viewdef(c.oid) AS definition,
			COALESCE((
				SELECT string_agg(a.attname, ',' ORDER BY a.attnum)
				FROM pg_attribute a
				WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
			), '') AS columns_csv
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('v', 'm')
		ORDER BY c.relname;
	`
	viewRows, err := dbtx.QueryContext(ctx, viewQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting views in schema %q: %w", schemaName, err)
	}
	defer func() { _ = viewRows.Close() }()

	for viewRows.Next() {
		var viewName, viewKind, definition, columnsCSV string
		if err := viewRows.Scan(&viewName, &viewKind, &definition, &columnsCSV); err != nil {
			return fmt.Errorf("scanning view in schema %q: %w", schemaName, err)
		}
		if s.Views == nil {
			s.Views = make(map[string]*schema.View)
		}
		v := &schema.View{
			Name:       viewName,
			IsMatView:  viewKind == "m",
			Definition: definition,
		}
		if columnsCSV != "" {
			v.Columns = strings.Split(columnsCSV, ",")
		}
		s.Views[viewName] = v
	}
	if err := viewRows.Err(); err != nil {
		return err
	}
	_ = viewRows.Close()

	// View dependencies via pg_depend.
	// Querying pg_depend directly recovers the DAG without fragile SQL parsing.
	// Note: PL/pgSQL function bodies are not parsed into pg_depend dependency edges by PostgreSQL,
	// so function-internal references cannot be ordered through catalog inspection.
	if len(s.Views) > 0 {
		viewDepQuery := `
			SELECT DISTINCT
				v.relname AS view_name,
				ref.relname AS ref_name,
				ref_n.nspname AS ref_schema
			FROM pg_depend d
			JOIN pg_rewrite r ON r.oid = d.objid
			JOIN pg_class v ON v.oid = r.ev_class
			JOIN pg_class ref ON ref.oid = d.refobjid
			JOIN pg_namespace n ON n.oid = v.relnamespace
			JOIN pg_namespace ref_n ON ref_n.oid = ref.relnamespace
			WHERE d.classid = 'pg_rewrite'::regclass
			  AND d.refclassid = 'pg_class'::regclass
			  AND v.relkind IN ('v', 'm')
			  AND ref.relkind IN ('v', 'm')
			  AND v.oid != ref.oid
			  AND n.nspname = $1
			UNION
			SELECT DISTINCT
				v.relname AS view_name,
				p.proname AS ref_name,
				pn.nspname AS ref_schema
			FROM pg_depend d
			JOIN pg_rewrite r ON r.oid = d.objid
			JOIN pg_class v ON v.oid = r.ev_class
			JOIN pg_proc p ON p.oid = d.refobjid
			JOIN pg_namespace n ON n.oid = v.relnamespace
			JOIN pg_namespace pn ON pn.oid = p.pronamespace
			WHERE d.classid = 'pg_rewrite'::regclass
			  AND d.refclassid = 'pg_proc'::regclass
			  AND v.relkind IN ('v', 'm')
			  AND n.nspname = $1
			  AND pn.nspname NOT IN ('pg_catalog', 'information_schema')
			ORDER BY view_name, ref_name;
		`
		viewDepRows, err := dbtx.QueryContext(ctx, viewDepQuery, schemaName)
		if err != nil {
			return fmt.Errorf("inspecting view dependencies in schema %q: %w", schemaName, err)
		}
		defer func() { _ = viewDepRows.Close() }()

		for viewDepRows.Next() {
			var viewName, refName, refSchema string
			if err := viewDepRows.Scan(&viewName, &refName, &refSchema); err != nil {
				return fmt.Errorf("scanning view dependency in schema %q: %w", schemaName, err)
			}
			if v, ok := s.Views[viewName]; ok {
				refKey := refSchema + "." + refName
				v.DependsOn = append(v.DependsOn, refKey)
			}
		}
		if err := viewDepRows.Err(); err != nil {
			return err
		}
		_ = viewDepRows.Close()

		for _, v := range s.Views {
			if len(v.DependsOn) > 1 {
				slices.Sort(v.DependsOn)
				v.DependsOn = slices.Compact(v.DependsOn)
			}
		}
	}

	return nil
}
