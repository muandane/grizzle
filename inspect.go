package grizzle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// inspectSchema reads the relational state of the specified schema directly from pg_catalog.
func inspectSchema(ctx context.Context, tx *sql.Tx, schemaName string) (*SchemaIR, error) {
	schema := &SchemaIR{
		Name:   schemaName,
		Tables: make(map[string]*TableIR),
	}

	// 1. Inspect Tables and Columns
	colQuery := `
		SELECT
			c.relname AS table_name,
			a.attname AS column_name,
			format_type(a.atttypid, a.atttypmod) AS formatted_type,
			NOT a.attnotnull AS is_nullable,
			COALESCE(pg_get_expr(d.adbin, d.adrelid), '') AS column_default,
			a.attnum AS ordinal_position,
			a.attidentity AS identity_type
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		WHERE n.nspname = $1
		  AND c.relkind = 'r'       -- Base tables only
		  AND a.attnum > 0          -- Filter out system columns (tableoid, ctid, etc.)
		  AND NOT a.attisdropped    -- Filter out dropped columns
		ORDER BY c.relname, a.attnum;
	`

	rows, err := tx.QueryContext(ctx, colQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting columns in schema %q: %w", schemaName, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			tableName    string
			colName      string
			rawType      string
			isNullable   bool
			rawDefault   string
			position     int
			identityType string
		)

		if err := rows.Scan(&tableName, &colName, &rawType, &isNullable, &rawDefault, &position, &identityType); err != nil {
			return nil, fmt.Errorf("scanning column data in schema %q: %w", schemaName, err)
		}

		tbl, exists := schema.Tables[tableName]
		if !exists {
			tbl = &TableIR{
				Name:    tableName,
				Columns: make(map[string]*ColumnIR),
			}
			schema.Tables[tableName] = tbl
		}

		col := &ColumnIR{
			Name:         colName,
			DataType:     normalizeType(rawType),
			IsNullable:   isNullable,
			DefaultValue: normalizeDefault(rawDefault),
			Position:     position,
		}

		switch identityType {
		case "a":
			col.IsIdentity = true
			col.IdentityType = "ALWAYS"
		case "d":
			col.IsIdentity = true
			col.IdentityType = "BY DEFAULT"
		}

		tbl.Columns[colName] = col
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()

	// 2. Inspect Primary Keys
	pkQuery := `
		SELECT
			c.relname AS table_name,
			con.conname AS constraint_name,
			COALESCE(string_agg(a.attname, ',' ORDER BY u.pos), '') AS pk_columns
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS u(attnum, pos)
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = u.attnum
		WHERE n.nspname = $1
		  AND con.contype = 'p'
		GROUP BY c.relname, con.conname;
	`

	pkRows, err := tx.QueryContext(ctx, pkQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting primary keys in schema %q: %w", schemaName, err)
	}
	defer func() { _ = pkRows.Close() }()

	for pkRows.Next() {
		var (
			tableName  string
			pkName     string
			colsJoined string
		)
		if err := pkRows.Scan(&tableName, &pkName, &colsJoined); err != nil {
			return nil, fmt.Errorf("scanning primary key in schema %q: %w", schemaName, err)
		}

		if tbl, exists := schema.Tables[tableName]; exists && colsJoined != "" {
			var pkCols []string
			for c := range strings.SplitSeq(colsJoined, ",") {
				pkCols = append(pkCols, strings.TrimSpace(c))
			}
			tbl.PrimaryKey = &PrimaryKeyIR{
				Name:    pkName,
				Columns: pkCols,
			}
		}
	}
	if err := pkRows.Err(); err != nil {
		return nil, err
	}

	return schema, nil
}
