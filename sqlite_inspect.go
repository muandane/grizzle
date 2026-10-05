package grizzle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type sqliteQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// inspectSQLiteSchema extracts the complete relational schema from an SQLite database connection.
func inspectSQLiteSchema(ctx context.Context, q sqliteQuerier) (*SchemaIR, error) {
	schema := &SchemaIR{
		Name:   "main",
		Tables: make(map[string]*TableIR),
		Enums:  make(map[string]*EnumIR),
	}

	// 1. Get Table names
	tblRows, err := q.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '_grizzle_%' ORDER BY name;")
	if err != nil {
		return nil, fmt.Errorf("sqlite: querying tables: %w", err)
	}
	defer func() { _ = tblRows.Close() }()

	var tableNames []string
	for tblRows.Next() {
		var name string
		if err := tblRows.Scan(&name); err != nil {
			return nil, err
		}
		tableNames = append(tableNames, name)
	}
	_ = tblRows.Close()

	for _, tblName := range tableNames {
		tbl := &TableIR{
			Name:        tblName,
			Columns:     make(map[string]*ColumnIR),
			Indexes:     make(map[string]*IndexIR),
			ForeignKeys: make(map[string]*ForeignKeyIR),
		}
		schema.Tables[tblName] = tbl

		// 2. Query columns using PRAGMA table_info
		colRows, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%q);", tblName))
		if err != nil {
			return nil, fmt.Errorf("sqlite: querying columns for %q: %w", tblName, err)
		}

		var pkCols []string
		for colRows.Next() {
			var (
				cid       int
				name      string
				colType   string
				notnull   int
				dfltValue sql.NullString
				pk        int
			)
			if err := colRows.Scan(&cid, &name, &colType, &notnull, &dfltValue, &pk); err != nil {
				_ = colRows.Close()
				return nil, err
			}

			normType := normalizeSQLiteType(colType)
			defVal := ""
			if dfltValue.Valid {
				defVal = normalizeSQLiteDefault(dfltValue.String)
			}

			tbl.Columns[name] = &ColumnIR{
				Name:         name,
				DataType:     normType,
				IsNullable:   notnull == 0,
				DefaultValue: defVal,
				Position:     cid,
			}

			if pk > 0 {
				pkCols = append(pkCols, name)
			}
		}
		_ = colRows.Close()

		if len(pkCols) > 0 {
			tbl.PrimaryKey = &PrimaryKeyIR{
				Name:    tblName + "_pkey",
				Columns: pkCols,
			}
		}

		// 3. Query Foreign Keys using PRAGMA foreign_key_list
		fkRows, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA foreign_key_list(%q);", tblName))
		if err != nil {
			return nil, fmt.Errorf("sqlite: querying foreign keys for %q: %w", tblName, err)
		}

		type fkGroup struct {
			refTable string
			fromCols []string
			toCols   []string
			onUpdate string
			onDelete string
		}
		fks := make(map[int]*fkGroup)

		for fkRows.Next() {
			var (
				id, seq  int
				refTable string
				fromCol  string
				toCol    string
				onUpdate string
				onDelete string
				match    string
			)
			if err := fkRows.Scan(&id, &seq, &refTable, &fromCol, &toCol, &onUpdate, &onDelete, &match); err != nil {
				_ = fkRows.Close()
				return nil, err
			}
			grp, exists := fks[id]
			if !exists {
				grp = &fkGroup{
					refTable: refTable,
					onUpdate: onUpdate,
					onDelete: onDelete,
				}
				fks[id] = grp
			}
			grp.fromCols = append(grp.fromCols, fromCol)
			grp.toCols = append(grp.toCols, toCol)
		}
		_ = fkRows.Close()

		for id, grp := range fks {
			fkName := fmt.Sprintf("fk_%s_%s_%d", tblName, grp.refTable, id)
			def := fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s(%s)",
				strings.Join(grp.fromCols, ", "),
				grp.refTable,
				strings.Join(grp.toCols, ", "),
			)
			if grp.onDelete != "" && !strings.EqualFold(grp.onDelete, "NO ACTION") {
				def += " ON DELETE " + grp.onDelete
			}
			if grp.onUpdate != "" && !strings.EqualFold(grp.onUpdate, "NO ACTION") {
				def += " ON UPDATE " + grp.onUpdate
			}
			tbl.ForeignKeys[fkName] = &ForeignKeyIR{
				Name:       fkName,
				TableName:  tblName,
				Definition: def,
			}
		}

		// 4. Query Indexes from sqlite_schema
		idxRows, err := q.QueryContext(ctx, "SELECT name, sql FROM sqlite_schema WHERE type='index' AND sql IS NOT NULL AND tbl_name = $1 AND name NOT LIKE 'sqlite_%';", tblName)
		if err != nil {
			return nil, fmt.Errorf("sqlite: querying indexes for %q: %w", tblName, err)
		}

		for idxRows.Next() {
			var name, indexSql string
			if err := idxRows.Scan(&name, &indexSql); err != nil {
				_ = idxRows.Close()
				return nil, err
			}
			isUnique := strings.Contains(strings.ToUpper(indexSql), "CREATE UNIQUE INDEX")
			tbl.Indexes[name] = &IndexIR{
				Name:       name,
				TableName:  tblName,
				IsUnique:   isUnique,
				Definition: indexSql,
			}
		}
		_ = idxRows.Close()
	}

	return schema, nil
}

func normalizeSQLiteType(t string) string {
	s := strings.TrimSpace(strings.ToUpper(t))
	if s == "" {
		return "TEXT"
	}
	switch s {
	case "INT", "INTEGER", "BIGINT", "TINYINT", "SMALLINT", "MEDIUMINT":
		return "INTEGER"
	case "CHARACTER", "VARCHAR", "VARYING CHARACTER", "NCHAR", "NATIVE CHARACTER", "NVARCHAR", "TEXT", "CLOB":
		return "TEXT"
	case "REAL", "DOUBLE", "DOUBLE PRECISION", "FLOAT":
		return "REAL"
	case "BOOLEAN", "BOOL":
		return "INTEGER" // SQLite stores booleans as integer 0 or 1
	case "BLOB":
		return "BLOB"
	case "NUMERIC", "DECIMAL":
		return "NUMERIC"
	default:
		return s
	}
}

func normalizeSQLiteDefault(d string) string {
	return strings.TrimSpace(d)
}
