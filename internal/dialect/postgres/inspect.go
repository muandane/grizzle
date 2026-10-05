package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/yourorg/grizzle/internal/dialect"
	"github.com/yourorg/grizzle/internal/schema"
)

// Inspect reads the relational state of the specified schema directly from pg_catalog.
func Inspect(ctx context.Context, dbtx dialect.DBTX, schemaName string) (*schema.Schema, error) {
	s := &schema.Schema{
		Name:   schemaName,
		Tables: make(map[string]*schema.Table),
		Enums:  make(map[string]*schema.Enum),
	}

	// 1. Inspect Custom ENUM Types
	enumQuery := `
		SELECT
			t.typname AS enum_name,
			e.enumlabel AS enum_value
		FROM pg_type t
		JOIN pg_enum e ON e.enumtypid = t.oid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = $1
		ORDER BY t.typname, e.enumsortorder;
	`
	enumRows, err := dbtx.QueryContext(ctx, enumQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting enums in schema %q: %w", schemaName, err)
	}
	defer func() { _ = enumRows.Close() }()

	for enumRows.Next() {
		var enumName, enumVal string
		if err := enumRows.Scan(&enumName, &enumVal); err != nil {
			return nil, fmt.Errorf("scanning enum in schema %q: %w", schemaName, err)
		}
		e, exists := s.Enums[enumName]
		if !exists {
			e = &schema.Enum{Name: enumName}
			s.Enums[enumName] = e
		}
		e.Values = append(e.Values, enumVal)
	}
	if err := enumRows.Err(); err != nil {
		return nil, err
	}
	_ = enumRows.Close()

	// 2. Inspect Tables and Columns
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

	rows, err := dbtx.QueryContext(ctx, colQuery, schemaName)
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

		tbl, exists := s.Tables[tableName]
		if !exists {
			tbl = &schema.Table{
				Name:        tableName,
				Columns:     make(map[string]*schema.Column),
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			}
			s.Tables[tableName] = tbl
		}

		cleanedType := strings.TrimPrefix(rawType, schemaName+".")
		cleanedType = strings.TrimPrefix(cleanedType, `"`+schemaName+`".`)

		col := &schema.Column{
			Name:         colName,
			DataType:     schema.NormalizeType(cleanedType),
			IsNullable:   isNullable,
			DefaultValue: schema.NormalizeDefault(rawDefault),
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

	// 3. Inspect Primary Keys
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

	pkRows, err := dbtx.QueryContext(ctx, pkQuery, schemaName)
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

		if tbl, exists := s.Tables[tableName]; exists && colsJoined != "" {
			var pkCols []string
			for c := range strings.SplitSeq(colsJoined, ",") {
				pkCols = append(pkCols, strings.TrimSpace(c))
			}
			tbl.PrimaryKey = &schema.PrimaryKey{
				Name:    pkName,
				Columns: pkCols,
			}
		}
	}
	if err := pkRows.Err(); err != nil {
		return nil, err
	}
	_ = pkRows.Close()

	// 4. Inspect Indexes (excluding primary keys)
	idxQuery := `
		SELECT
			t.relname AS table_name,
			i.relname AS index_name,
			ix.indisunique AS is_unique,
			ix.indisvalid AS is_valid,
			pg_get_indexdef(ix.indexrelid) AS index_def
		FROM pg_index ix
		JOIN pg_class t ON t.oid = ix.indrelid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1
		  AND NOT ix.indisprimary
		ORDER BY t.relname, i.relname;
	`
	idxRows, err := dbtx.QueryContext(ctx, idxQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting indexes in schema %q: %w", schemaName, err)
	}
	defer func() { _ = idxRows.Close() }()

	for idxRows.Next() {
		var (
			tableName string
			indexName string
			isUnique  bool
			isValid   bool
			indexDef  string
		)
		if err := idxRows.Scan(&tableName, &indexName, &isUnique, &isValid, &indexDef); err != nil {
			return nil, fmt.Errorf("scanning index in schema %q: %w", schemaName, err)
		}

		if tbl, exists := s.Tables[tableName]; exists {
			tbl.Indexes[indexName] = &schema.Index{
				Name:       indexName,
				TableName:  tableName,
				IsUnique:   isUnique,
				Definition: indexDef,
				IsValid:    isValid,
			}
		}
	}
	if err := idxRows.Err(); err != nil {
		return nil, err
	}
	_ = idxRows.Close()

	// 5. Inspect Foreign Keys
	fkQuery := `
		SELECT
			c.relname AS table_name,
			con.conname AS constraint_name,
			pg_get_constraintdef(con.oid) AS constraint_def,
			con.convalidated AS is_valid
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND con.contype = 'f'
		ORDER BY c.relname, con.conname;
	`
	fkRows, err := dbtx.QueryContext(ctx, fkQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting foreign keys in schema %q: %w", schemaName, err)
	}
	defer func() { _ = fkRows.Close() }()

	for fkRows.Next() {
		var (
			tableName string
			fkName    string
			fkDef     string
			isValid   bool
		)
		if err := fkRows.Scan(&tableName, &fkName, &fkDef, &isValid); err != nil {
			return nil, fmt.Errorf("scanning foreign key in schema %q: %w", schemaName, err)
		}

		if tbl, exists := s.Tables[tableName]; exists {
			tbl.ForeignKeys[fkName] = &schema.ForeignKey{
				Name:       fkName,
				TableName:  tableName,
				Definition: fkDef,
				IsValid:    isValid,
			}
		}
	}
	if err := fkRows.Err(); err != nil {
		return nil, err
	}

	return s, nil
}
