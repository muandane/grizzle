package postgres

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/schema"
)

// inspectRoutines inspects user-defined aggregates, functions, procedures, their dependencies, and unmanaged routines.
func inspectRoutines(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	// 1. Managed aggregates (prokind = 'a').
	aggregateQuery := `
		SELECT
			p.proname AS agg_name,
			pg_get_function_identity_arguments(p.oid) AS identity_args,
			format_type(a.aggtranstype, NULL) AS state_type,
			a.aggtransfn::regprocedure::text AS sfunc,
			COALESCE(NULLIF(a.aggfinalfn::regprocedure::text, '-'), '') AS finalfunc,
			COALESCE(NULLIF(a.aggcombinefn::regprocedure::text, '-'), '') AS combinefunc,
			COALESCE(NULLIF(a.aggserialfn::regprocedure::text, '-'), '') AS serialfunc,
			COALESCE(NULLIF(a.aggdeserialfn::regprocedure::text, '-'), '') AS deserialfunc,
			COALESCE(a.agginitval, '') AS initcond,
			CASE p.proparallel WHEN 's' THEN 'SAFE' WHEN 'r' THEN 'RESTRICTED' ELSE 'UNSAFE' END AS parallel
		FROM pg_proc p
		JOIN pg_aggregate a ON a.aggfnoid = p.oid
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1
		  AND p.prokind = 'a'
		  AND a.aggkind = 'n'
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = p.oid AND d.deptype = 'e'
		  )
		ORDER BY p.proname, identity_args;
	`
	aggRows, err := dbtx.QueryContext(ctx, aggregateQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting aggregates in schema %q: %w", schemaName, err)
	}
	for aggRows.Next() {
		var name, identityArgs, stype, sfunc, parallel string
		var finalfunc, combinefunc, serialfunc, deserialfunc, initcond string
		if err := aggRows.Scan(&name, &identityArgs, &stype, &sfunc, &finalfunc, &combinefunc, &serialfunc, &deserialfunc, &initcond, &parallel); err != nil {
			_ = aggRows.Close()
			return fmt.Errorf("scanning aggregate in schema %q: %w", schemaName, err)
		}
		if s.Routines == nil {
			s.Routines = make(map[string]*schema.Routine)
		}
		s.Routines[schema.RoutineKey(name, identityArgs)] = &schema.Routine{
			Name:         name,
			Kind:         "AGGREGATE",
			IdentityArgs: identityArgs,
			Definition:   reconstructAggregateDefinition(name, identityArgs, stype, sfunc, finalfunc, combinefunc, serialfunc, deserialfunc, initcond, parallel),
		}
	}
	if err := aggRows.Err(); err != nil {
		return err
	}
	_ = aggRows.Close()

	// 2. Managed routines (functions and procedures).
	routineQuery := `
		SELECT
			p.proname AS routine_name,
			pg_get_function_identity_arguments(p.oid) AS identity_args,
			COALESCE(pg_get_function_result(p.oid), '') AS return_type,
			l.lanname AS language,
			CASE p.provolatile WHEN 'i' THEN 'IMMUTABLE' WHEN 's' THEN 'STABLE' ELSE 'VOLATILE' END AS volatility,
			p.prosecdef AS security_definer,
			pg_get_functiondef(p.oid) AS definition,
			p.prokind AS routine_kind
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		JOIN pg_language l ON l.oid = p.prolang
		WHERE n.nspname = $1
		  AND p.prokind IN ('f', 'p')
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = p.oid AND d.deptype = 'e'
		  )
		ORDER BY p.proname, identity_args;
	`
	routineRows, err := dbtx.QueryContext(ctx, routineQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting routines in schema %q: %w", schemaName, err)
	}
	for routineRows.Next() {
		var name, identityArgs, returnType, lang, volatility, definition, routineKind string
		var securityDefiner bool
		if err := routineRows.Scan(&name, &identityArgs, &returnType, &lang, &volatility, &securityDefiner, &definition, &routineKind); err != nil {
			_ = routineRows.Close()
			return fmt.Errorf("scanning routine in schema %q: %w", schemaName, err)
		}
		kind := "FUNCTION"
		if routineKind == "p" {
			kind = "PROCEDURE"
		}
		if s.Routines == nil {
			s.Routines = make(map[string]*schema.Routine)
		}
		s.Routines[schema.RoutineKey(name, identityArgs)] = &schema.Routine{
			Name:            name,
			Kind:            kind,
			IdentityArgs:    identityArgs,
			ReturnType:      returnType,
			Language:        lang,
			Volatility:      volatility,
			SecurityDefiner: securityDefiner,
			Definition:      definition,
		}
	}
	if err := routineRows.Err(); err != nil {
		_ = routineRows.Close()
		return err
	}
	_ = routineRows.Close()

	// 3. Routine dependencies on custom user types via pg_depend.
	// Note: PL/pgSQL function bodies are not parsed into pg_depend dependency edges by PostgreSQL,
	// so function-internal references cannot be ordered through catalog inspection.
	if len(s.Routines) > 0 {
		routineDepQuery := `
			SELECT DISTINCT
				p.proname AS routine_name,
				pg_get_function_identity_arguments(p.oid) AS identity_args,
				t.typname AS ref_name,
				tn.nspname AS ref_schema
			FROM pg_depend d
			JOIN pg_proc p ON p.oid = d.objid
			JOIN pg_type t ON t.oid = d.refobjid
			JOIN pg_namespace pn ON pn.oid = p.pronamespace
			JOIN pg_namespace tn ON tn.oid = t.typnamespace
			WHERE d.classid = 'pg_proc'::regclass
			  AND d.refclassid = 'pg_type'::regclass
			  AND pn.nspname = $1
			  AND tn.nspname NOT IN ('pg_catalog', 'information_schema')
			  AND t.typname NOT LIKE '\_%'
			ORDER BY routine_name, identity_args;
		`
		routineDepRows, err := dbtx.QueryContext(ctx, routineDepQuery, schemaName)
		if err != nil {
			return fmt.Errorf("inspecting routine dependencies in schema %q: %w", schemaName, err)
		}
		defer func() { _ = routineDepRows.Close() }()

		for routineDepRows.Next() {
			var rName, identityArgs, refName, refSchema string
			if err := routineDepRows.Scan(&rName, &identityArgs, &refName, &refSchema); err != nil {
				return fmt.Errorf("scanning routine dependency in schema %q: %w", schemaName, err)
			}
			key := schema.RoutineKey(rName, identityArgs)
			if r, ok := s.Routines[key]; ok {
				refKey := refSchema + "." + refName
				r.DependsOn = append(r.DependsOn, refKey)
			}
		}
		if err := routineDepRows.Err(); err != nil {
			return err
		}
		_ = routineDepRows.Close()

		for _, r := range s.Routines {
			if len(r.DependsOn) > 1 {
				slices.Sort(r.DependsOn)
				r.DependsOn = slices.Compact(r.DependsOn)
			}
		}
	}

	// 4. Unmanaged window functions and special aggregates.
	unmanagedProcQuery := `
		SELECT
			p.proname AS proc_name,
			COALESCE(p.prosrc, '') AS source
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1
		  AND (
		      p.prokind = 'w'
		      OR (p.prokind = 'a' AND EXISTS (
		          SELECT 1 FROM pg_aggregate a
		          WHERE a.aggfnoid = p.oid AND a.aggkind <> 'n'
		      ))
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = p.oid AND d.deptype = 'e'
		  )
		ORDER BY p.proname;
	`
	procRows, err := dbtx.QueryContext(ctx, unmanagedProcQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting procedures in schema %q: %w", schemaName, err)
	}
	defer func() { _ = procRows.Close() }()

	procSources := make(map[string]string)
	for procRows.Next() {
		var procName, procSource string
		if err := procRows.Scan(&procName, &procSource); err != nil {
			return fmt.Errorf("scanning procedure in schema %q: %w", schemaName, err)
		}
		key := "function:" + procName
		s.Unmanaged[key] = &schema.UnmanagedObject{
			Name: procName,
			Kind: schema.UnmanagedFunction,
		}
		procSources[procName] = procSource
	}
	if err := procRows.Err(); err != nil {
		return err
	}
	_ = procRows.Close()

	// 5. Function Table/Column Dependencies from pg_depend.
	funcDepQuery := `
		SELECT
			p.proname AS proc_name,
			dep_c.relname AS referenced_table,
			COALESCE(a.attname, '') AS referenced_column
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		JOIN pg_depend d ON d.objid = p.oid AND d.classid = 'pg_proc'::regclass AND d.refclassid = 'pg_class'::regclass
		JOIN pg_class dep_c ON dep_c.oid = d.refobjid
		LEFT JOIN pg_attribute a ON a.attrelid = dep_c.oid AND a.attnum = d.refobjsubid AND NOT a.attisdropped
		WHERE n.nspname = $1
		  AND d.deptype = 'n'
		  AND dep_c.relkind IN ('r', 'p')
		ORDER BY p.proname, dep_c.relname, a.attname;
	`
	funcDepRows, err := dbtx.QueryContext(ctx, funcDepQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting function dependencies in schema %q: %w", schemaName, err)
	}
	defer func() { _ = funcDepRows.Close() }()

	funcsWithDeps := make(map[string]bool)
	seenFuncDeps := make(map[string]bool)
	for funcDepRows.Next() {
		var procName, refTable, refCol string
		if err := funcDepRows.Scan(&procName, &refTable, &refCol); err != nil {
			return fmt.Errorf("scanning function dependency: %w", err)
		}
		obj, ok := s.Unmanaged["function:"+procName]
		if !ok {
			continue
		}
		dedupeKey := procName + "\x00" + refTable + "\x00" + refCol
		if seenFuncDeps[dedupeKey] {
			continue
		}
		seenFuncDeps[dedupeKey] = true
		obj.DependsOn = append(obj.DependsOn, schema.DependencyRef{
			Table:  refTable,
			Column: refCol,
		})
		funcsWithDeps[procName] = true
	}
	if err := funcDepRows.Err(); err != nil {
		return err
	}
	_ = funcDepRows.Close()

	// 6. Heuristic source scan for string-bodied functions.
	for _, procName := range sortedProcNames(procSources) {
		if funcsWithDeps[procName] {
			continue
		}
		source := procSources[procName]
		if strings.TrimSpace(source) == "" {
			continue
		}
		obj, ok := s.Unmanaged["function:"+procName]
		if !ok {
			continue
		}
		for _, tblName := range sortedManagedTableNames(s) {
			if tblName == "" {
				continue
			}
			re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(tblName) + `\b`)
			if re.MatchString(source) {
				obj.DependsOn = append(obj.DependsOn, schema.DependencyRef{Table: tblName})
			}
		}
	}

	return nil
}

// reconstructAggregateDefinition rebuilds a canonical CREATE AGGREGATE
// statement from pg_aggregate catalog fields.
func reconstructAggregateDefinition(name, identityArgs, stype, sfunc, finalfunc, combinefunc, serialfunc, deserialfunc, initcond, parallel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE AGGREGATE %s(%s) (\n", name, identityArgs)
	parts := []string{
		fmt.Sprintf("    SFUNC = %s", sfunc),
		fmt.Sprintf("    STYPE = %s", stype),
	}
	if finalfunc != "" {
		parts = append(parts, fmt.Sprintf("    FINALFUNC = %s", finalfunc))
	}
	if combinefunc != "" {
		parts = append(parts, fmt.Sprintf("    COMBINEFUNC = %s", combinefunc))
	}
	if serialfunc != "" {
		parts = append(parts, fmt.Sprintf("    SERIALFUNC = %s", serialfunc))
	}
	if deserialfunc != "" {
		parts = append(parts, fmt.Sprintf("    DESERIALFUNC = %s", deserialfunc))
	}
	if initcond != "" {
		parts = append(parts, fmt.Sprintf("    INITCOND = '%s'", initcond))
	}
	parts = append(parts, fmt.Sprintf("    PARALLEL = %s", parallel))
	b.WriteString(strings.Join(parts, ",\n"))
	b.WriteString("\n);")
	return b.String()
}

// sortedProcNames returns proc source keys in deterministic order.
func sortedProcNames(procSources map[string]string) []string {
	names := make([]string, 0, len(procSources))
	for n := range procSources {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// sortedManagedTableNames returns managed table names in deterministic order.
func sortedManagedTableNames(s *schema.Schema) []string {
	names := make([]string, 0, len(s.Tables))
	for n := range s.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
