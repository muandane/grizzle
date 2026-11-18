package postgres

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/schema"
)

// pinSearchPath pins search_path so the inspected schema precedes target schemas, public, and pg_catalog
// for the remainder of the current transaction so pg_get_*def / pg_get_expr emit clean, unqualified definitions for objects
// in the active schema, while ensuring catalog and extension types resolve reliably. It is a no-op when dbtx is not inside
// a transaction (e.g. direct *sql.DB inspection, where the statement-scoped
// pin is impossible and the session path is whatever the pool provides).
// Call unpinSearchPath after the pin-affected queries to restore the
// transaction path.
func pinSearchPath(ctx context.Context, dbtx dialect.DBTX, schemaName string, searchPath ...string) {
	var paths []string
	paths = append(paths, fmt.Sprintf("%q", schemaName))
	for _, p := range searchPath {
		if p != "" && p != schemaName {
			paths = append(paths, fmt.Sprintf("%q", p))
		}
	}
	_, _ = dbtx.ExecContext(ctx, fmt.Sprintf("SAVEPOINT grizzle_inspect_pin; SET search_path TO %s;", strings.Join(paths, ", ")))
}

// unpinSearchPath reverts the pinned path (ROLLBACK TO SAVEPOINT undoes the
// SET LOCAL effect) and discards the savepoint (RELEASE).
func unpinSearchPath(ctx context.Context, dbtx dialect.DBTX) {
	_, _ = dbtx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_inspect_pin; RELEASE SAVEPOINT grizzle_inspect_pin;")
}

// Inspect reads the relational state of the specified schema directly from pg_catalog.
func Inspect(ctx context.Context, dbtx dialect.DBTX, schemaName string, searchPath ...string) (*schema.Schema, error) {
	pinSearchPath(ctx, dbtx, schemaName, searchPath...)
	defer unpinSearchPath(ctx, dbtx)

	s := &schema.Schema{
		Name:           schemaName,
		Tables:         make(map[string]*schema.Table),
		Enums:          make(map[string]*schema.Enum),
		Extensions:     make(map[string]*schema.Extension),
		Routines:       make(map[string]*schema.Routine),
		RangeTypes:     make(map[string]*schema.RangeType),
		CompositeTypes: make(map[string]*schema.CompositeType),
		Sequences:      make(map[string]*schema.Sequence),
		Views:          make(map[string]*schema.View),
		Unmanaged:      make(map[string]*schema.UnmanagedObject),
	}

	// 0. Inspect Extensions (database-wide)
	if err := inspectExtensions(ctx, dbtx, s); err != nil {
		return nil, err
	}

	// 1. Inspect Custom ENUM Types
	if err := inspectEnums(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 2. Inspect Tables and Columns (including table comments)
	if err := inspectTablesAndColumns(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 3. Inspect Partitioned Tables and Attached Partitions
	if err := inspectPartitions(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 4. Inspect Row-Level Security Flags and Policies
	if err := inspectRLS(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 5. Inspect Triggers on Tables
	if err := inspectTriggers(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 6. Inspect Primary Keys, Indexes, Foreign Keys, and Check Constraints
	if err := inspectConstraintsAndIndexes(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 7. Inspect Views and Materialized Views (with pg_depend dependencies)
	if err := inspectViews(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 8. Inspect Routines (aggregates, functions, procedures, dependencies)
	if err := inspectRoutines(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 9. Inspect Sequences (managed and unmanaged)
	if err := inspectSequences(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 10. Inspect Managed Domains and Domain CHECK Constraints
	if err := inspectDomains(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	// 11. Inspect Custom Range Types and Custom Composite Types
	if err := inspectTypes(ctx, dbtx, schemaName, s); err != nil {
		return nil, err
	}

	return s, nil
}

// InspectSchemas reads the relational states across all specified schema names.
func InspectSchemas(ctx context.Context, dbtx dialect.DBTX, schemaNames []string, searchPath ...string) (map[string]*schema.Schema, error) {
	schemas := make(map[string]*schema.Schema, len(schemaNames))
	for _, name := range schemaNames {
		s, err := Inspect(ctx, dbtx, name, searchPath...)
		if err != nil {
			return nil, err
		}
		schemas[name] = s
	}
	return schemas, nil
}

// DiscoverSchemas queries pg_namespace for schemas matching a LIKE pattern,
// excluding system schemas (pg_*, information_schema, and _grizzle*).
// The returned schema names are deduplicated and sorted deterministically.
func DiscoverSchemas(ctx context.Context, dbtx dialect.DBTX, pattern string) ([]string, error) {
	if dbtx == nil {
		return nil, fmt.Errorf("grizzle: database connection is nil")
	}
	const q = `
SELECT nspname FROM pg_namespace
WHERE nspname LIKE $1
  AND nspname NOT LIKE 'pg_%'
  AND nspname != 'information_schema'
  AND nspname NOT LIKE '_grizzle%'
ORDER BY nspname ASC;`

	rows, err := dbtx.QueryContext(ctx, q, pattern)
	if err != nil {
		return nil, fmt.Errorf("discovering schemas with pattern %q: %w", pattern, err)
	}
	defer func() { _ = rows.Close() }()

	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scanning discovered schema name: %w", err)
		}
		schemas = append(schemas, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating discovered schemas: %w", err)
	}
	slices.Sort(schemas)
	return slices.Compact(schemas), nil
}
