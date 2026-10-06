package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/schema"
)

// Inspect extracts the complete relational schema from an SQLite database connection.
func Inspect(ctx context.Context, dbtx dialect.DBTX) (*schema.Schema, error) {
	s := &schema.Schema{
		Name:   "main",
		Tables: make(map[string]*schema.Table),
		Enums:  make(map[string]*schema.Enum),
	}

	// 1. Get Table names and DDL SQL
	tblRows, err := dbtx.QueryContext(ctx, "SELECT name, sql FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '_grizzle_%' ORDER BY name;")
	if err != nil {
		return nil, fmt.Errorf("sqlite: querying tables: %w", err)
	}
	defer func() { _ = tblRows.Close() }()

	var tableNames []string
	tableSQLMap := make(map[string]string)
	for tblRows.Next() {
		var name string
		var tableSQL sql.NullString
		if err := tblRows.Scan(&name, &tableSQL); err != nil {
			return nil, err
		}
		tableNames = append(tableNames, name)
		if tableSQL.Valid {
			tableSQLMap[name] = tableSQL.String
		}
	}
	_ = tblRows.Close()

	for _, tblName := range tableNames {
		tbl := &schema.Table{
			Name:        tblName,
			Columns:     make(map[string]*schema.Column),
			Indexes:     make(map[string]*schema.Index),
			ForeignKeys: make(map[string]*schema.ForeignKey),
		}
		s.Tables[tblName] = tbl
		tableDDL := tableSQLMap[tblName]

		// 2. Query columns using PRAGMA table_xinfo (includes generated columns and hidden flag)
		colRows, err := dbtx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_xinfo(%q);", tblName))
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
				hidden    int
			)
			if err := colRows.Scan(&cid, &name, &colType, &notnull, &dfltValue, &pk, &hidden); err != nil {
				_ = colRows.Close()
				return nil, err
			}

			normType := NormalizeType(colType)
			defVal := ""
			if dfltValue.Valid {
				defVal = NormalizeDefault(dfltValue.String)
			}

			col := &schema.Column{
				Name:         name,
				DataType:     normType,
				IsNullable:   notnull == 0,
				DefaultValue: defVal,
				Position:     cid,
			}

			// hidden == 2 is VIRTUAL generated column, hidden == 3 is STORED generated column
			if hidden == 2 || hidden == 3 {
				expr := extractSQLiteGeneratedExpr(tableDDL, name)
				col.Generated = &schema.GeneratedColumn{
					Expr:   schema.NormalizeGeneratedExpr(expr),
					Stored: hidden == 3,
				}
				col.DefaultValue = ""
			}

			tbl.Columns[name] = col

			if pk > 0 {
				pkCols = append(pkCols, name)
			}
		}
		_ = colRows.Close()

		if len(pkCols) > 0 {
			tbl.PrimaryKey = &schema.PrimaryKey{
				Name:    tblName + "_pkey",
				Columns: pkCols,
			}
		}

		// 3. Query Foreign Keys using PRAGMA foreign_key_list
		fkRows, err := dbtx.QueryContext(ctx, fmt.Sprintf("PRAGMA foreign_key_list(%q);", tblName))
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
			tbl.ForeignKeys[fkName] = &schema.ForeignKey{
				Name:       fkName,
				TableName:  tblName,
				Definition: def,
			}
		}

		// 4. Query Indexes using PRAGMA index_list for structural uniqueness
		uniqueMap := make(map[string]bool)
		idxListRows, err := dbtx.QueryContext(ctx, fmt.Sprintf("PRAGMA index_list(%q);", tblName))
		if err == nil {
			for idxListRows.Next() {
				var (
					seq       int
					name      string
					uniqueVal int
					origin    string
					partial   int
				)
				if err := idxListRows.Scan(&seq, &name, &uniqueVal, &origin, &partial); err == nil {
					uniqueMap[name] = (uniqueVal == 1)
				}
			}
			_ = idxListRows.Close()
		}

		idxRows, err := dbtx.QueryContext(ctx, "SELECT name, sql FROM sqlite_schema WHERE type='index' AND sql IS NOT NULL AND tbl_name = $1 AND name NOT LIKE 'sqlite_%';", tblName)
		if err != nil {
			return nil, fmt.Errorf("sqlite: querying indexes for %q: %w", tblName, err)
		}

		for idxRows.Next() {
			var name, indexSql string
			if err := idxRows.Scan(&name, &indexSql); err != nil {
				_ = idxRows.Close()
				return nil, err
			}
			tbl.Indexes[name] = &schema.Index{
				Name:       name,
				TableName:  tblName,
				IsUnique:   uniqueMap[name],
				Definition: indexSql,
				IsValid:    true,
			}
		}
		_ = idxRows.Close()
	}

	return s, nil
}

// NormalizeType standardizes SQLite types into canonical representations.
func NormalizeType(t string) string {
	s := strings.TrimSpace(t)
	if s == "" {
		return "TEXT"
	}
	return strings.ToUpper(schema.NormalizeType(s))
}

// NormalizeDefault standardizes default expressions in SQLite.
func NormalizeDefault(d string) string {
	return strings.TrimSpace(d)
}

func extractSQLiteGeneratedExpr(tableSQL, colName string) string {
	pattern := fmt.Sprintf(`(?i)(?:["'`+"`"+`]%s["'`+"`"+`]|\b%s\b)[^,;]*?(?:GENERATED\s+ALWAYS\s+)?AS\s*\(`, regexp.QuoteMeta(colName), regexp.QuoteMeta(colName))
	re := regexp.MustCompile(pattern)
	loc := re.FindStringIndex(tableSQL)
	if loc == nil {
		return ""
	}
	start := loc[1] - 1 // points to '('
	count := 0
	inQuote := false
	for i := start; i < len(tableSQL); i++ {
		c := tableSQL[i]
		if c == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch c {
		case '(':
			count++
		case ')':
			count--
			if count == 0 {
				return schema.NormalizeGeneratedExpr(tableSQL[start+1 : i])
			}
		}
	}
	return ""
}
