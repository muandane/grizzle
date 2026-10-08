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

// Inspect extracts the complete relational schema from the primary ("main")
// SQLite database on the connection.
func Inspect(ctx context.Context, dbtx dialect.DBTX) (*schema.Schema, error) {
	return InspectSchema(ctx, dbtx, "main")
}

// InspectSchemas inspects each named schema (including attached databases) and
// returns a map keyed by schema name.
func InspectSchemas(ctx context.Context, dbtx dialect.DBTX, schemas []string) (map[string]*schema.Schema, error) {
	out := make(map[string]*schema.Schema, len(schemas))
	for _, name := range schemas {
		s, err := InspectSchema(ctx, dbtx, name)
		if err != nil {
			return nil, err
		}
		out[name] = s
	}
	return out, nil
}

// InspectSchema extracts the relational schema from a named SQLite schema
// (e.g. "main" or an ATTACH DATABASE name). Catalog reads use
// schema.sqlite_schema and PRAGMA schema.table_xinfo.
func InspectSchema(ctx context.Context, dbtx dialect.DBTX, schemaName string) (*schema.Schema, error) {
	if schemaName == "" {
		schemaName = "main"
	}
	s := &schema.Schema{
		Name:      schemaName,
		Tables:    make(map[string]*schema.Table),
		Enums:     make(map[string]*schema.Enum),
		Unmanaged: make(map[string]*schema.UnmanagedObject),
	}

	catalog := fmt.Sprintf("%q.sqlite_schema", schemaName)

	// 1. Get Table names and DDL SQL
	tblRows, err := dbtx.QueryContext(ctx, fmt.Sprintf(
		"SELECT name, sql FROM %s WHERE type='table' AND name NOT LIKE 'sqlite_%%' AND name NOT LIKE '_grizzle_%%' ORDER BY name;",
		catalog,
	))
	if err != nil {
		return nil, fmt.Errorf("sqlite: querying tables in schema %q: %w", schemaName, err)
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
			Schema:      schemaName,
			Name:        tblName,
			Columns:     make(map[string]*schema.Column),
			Indexes:     make(map[string]*schema.Index),
			ForeignKeys: make(map[string]*schema.ForeignKey),
		}
		s.Tables[tblName] = tbl
		tableDDL := tableSQLMap[tblName]

		// 1b. Parse CHECK constraints (named and inline) from the stored DDL.
		// SQLite has no PRAGMA for CHECK constraints; sqlite_schema.sql is the
		// only source. Both sides of the diff (live and in-memory shadow)
		// parse through this same path, keeping comparisons symmetric.
		tbl.Checks = parseSQLiteChecks(tblName, tableDDL)

		// 2. Query columns using PRAGMA schema.table_xinfo
		colRows, err := dbtx.QueryContext(ctx, fmt.Sprintf("PRAGMA %q.table_xinfo(%q);", schemaName, tblName))
		if err != nil {
			return nil, fmt.Errorf("sqlite: querying columns for %q.%q: %w", schemaName, tblName, err)
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
			// Preserve INTEGER PRIMARY KEY AUTOINCREMENT vs plain INTEGER PRIMARY KEY.
			if len(pkCols) == 1 && tableHasSQLiteAutoincrement(tableDDL) {
				if col := tbl.Columns[pkCols[0]]; col != nil && strings.EqualFold(col.DataType, "INTEGER") {
					col.Autoincrement = true
				}
			}
		}

		// 3. Query Foreign Keys using PRAGMA schema.foreign_key_list
		fkRows, err := dbtx.QueryContext(ctx, fmt.Sprintf("PRAGMA %q.foreign_key_list(%q);", schemaName, tblName))
		if err != nil {
			return nil, fmt.Errorf("sqlite: querying foreign keys for %q.%q: %w", schemaName, tblName, err)
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

		// 4. Query Indexes using PRAGMA schema.index_list for structural uniqueness
		uniqueMap := make(map[string]bool)
		idxListRows, err := dbtx.QueryContext(ctx, fmt.Sprintf("PRAGMA %q.index_list(%q);", schemaName, tblName))
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

		idxRows, err := dbtx.QueryContext(ctx, fmt.Sprintf(
			"SELECT name, sql FROM %s WHERE type='index' AND sql IS NOT NULL AND tbl_name = $1 AND name NOT LIKE 'sqlite_%%'",
			catalog,
		), tblName)
		if err != nil {
			return nil, fmt.Errorf("sqlite: querying indexes for %q.%q: %w", schemaName, tblName, err)
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
	}

	// 5. Query Triggers
	trigRows, err := dbtx.QueryContext(ctx, fmt.Sprintf(
		"SELECT name, tbl_name, sql FROM %s WHERE type='trigger' AND sql IS NOT NULL AND name NOT LIKE 'sqlite_%%'",
		catalog,
	))
	if err == nil {
		for trigRows.Next() {
			var name, tblName, sqlDef string
			if err := trigRows.Scan(&name, &tblName, &sqlDef); err == nil {
				s.Unmanaged["trigger:"+name] = &schema.UnmanagedObject{
					Name:  name,
					Kind:  schema.UnmanagedTrigger,
					Table: tblName,
					SQL:   sqlDef,
					DependsOn: []schema.DependencyRef{
						{Table: tblName},
					},
				}
			}
		}
		_ = trigRows.Close()
	}

	// 6. Query Views
	viewRows, err := dbtx.QueryContext(ctx, fmt.Sprintf(
		"SELECT name, sql FROM %s WHERE type='view' AND sql IS NOT NULL AND name NOT LIKE 'sqlite_%%'",
		catalog,
	))
	if err == nil {
		for viewRows.Next() {
			var name, sqlDef string
			if err := viewRows.Scan(&name, &sqlDef); err == nil {
				var refs []schema.DependencyRef
				for tblName := range s.Tables {
					pattern := fmt.Sprintf(`(?i)(?:["'`+"`"+`]%s["'`+"`"+`]|\b%s\b)`, regexp.QuoteMeta(tblName), regexp.QuoteMeta(tblName))
					if matched, _ := regexp.MatchString(pattern, sqlDef); matched {
						refs = append(refs, schema.DependencyRef{Table: tblName})
					}
				}
				s.Unmanaged["view:"+name] = &schema.UnmanagedObject{
					Name:      name,
					Kind:      schema.UnmanagedView,
					SQL:       sqlDef,
					DependsOn: refs,
				}
			}
		}
		_ = viewRows.Close()
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

var sqliteAutoincrementRe = regexp.MustCompile(`(?i)\bAUTOINCREMENT\b`)

func tableHasSQLiteAutoincrement(tableDDL string) bool {
	return sqliteAutoincrementRe.MatchString(tableDDL)
}

// sqliteCheckRe matches a table-level CHECK definition, optionally preceded by
// a named CONSTRAINT clause. Capture 1 is the constraint name (may be empty
// for bare CHECK) and capture 2 is the check keyword position anchor.
var sqliteCheckRe = regexp.MustCompile(`(?i)^(?:CONSTRAINT\s+(?:"([^"]+)"|(\w+))\s+)?CHECK\s*\(`)

var sqliteColumnRe = regexp.MustCompile(`^(?:"([^"]+)"|(\w+))\s`)

// sqliteInlineNameRe matches a trailing inline CONSTRAINT clause
// ("CONSTRAINT <name>") at the end of a fragment preceding an inline CHECK.
var sqliteInlineNameRe = regexp.MustCompile(`(?i)CONSTRAINT\s+(?:"([^"]+)"|(\w+))$`)

// parseSQLiteChecks extracts CHECK constraints from a CREATE TABLE statement.
// Table-level forms (CONSTRAINT <name> CHECK (...) and bare CHECK (...)) and
// column-level inline forms are captured. Bare and column-level checks are
// named deterministically (mirroring PostgreSQL auto-naming: column-level
// "<table>_<column>_check", table-level "<table>_check" with numeric
// disambiguation), so live and shadow parses of equivalent DDL produce
// identical constraint keys. Expression text is whitespace-normalized.
func parseSQLiteChecks(tableName, tableDDL string) map[string]*schema.CheckConstraint {
	if tableDDL == "" {
		return nil
	}
	body, ok := sqliteTableBody(tableDDL)
	if !ok {
		return nil
	}

	checks := make(map[string]*schema.CheckConstraint)
	autoSeq := 0
	for _, part := range splitSQLiteTopLevel(body) {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}

		// Named or bare table-level CHECK.
		if m := sqliteCheckRe.FindStringSubmatch(trimmed); m != nil {
			name := m[1] + m[2]
			expr, ok := balancedParenExpr(trimmed[strings.Index(trimmed, "("):])
			if !ok {
				continue
			}
			if name == "" {
				autoSeq++
				if autoSeq == 1 {
					name = tableName + "_check"
				} else {
					name = fmt.Sprintf("%s_check%d", tableName, autoSeq)
				}
			}
			checks[name] = &schema.CheckConstraint{
				Name:       name,
				TableName:  tableName,
				Definition: "CHECK (" + normalizeSQLiteCheckExpr(expr) + ")",
				IsValid:    true,
			}
			continue
		}

		// Column definitions may carry inline CHECKs, with or without a
		// CONSTRAINT name: "<col> <type> [CONSTRAINT <name>] CHECK (...)".
		colName := ""
		if cm := sqliteColumnRe.FindStringSubmatch(trimmed); cm != nil {
			colName = cm[1] + cm[2]
		}
		if colName == "" {
			continue
		}
		eachSQLiteCheckExpr(trimmed, func(expr string, name string) {
			if name == "" {
				name = fmt.Sprintf("%s_%s_check", tableName, colName)
				// Multiple inline checks on one column get numeric disambiguation.
				for suffix := 2; ; suffix++ {
					if _, exists := checks[name]; !exists {
						break
					}
					name = fmt.Sprintf("%s_%s_check%d", tableName, colName, suffix)
				}
			}
			checks[name] = &schema.CheckConstraint{
				Name:       name,
				TableName:  tableName,
				Definition: "CHECK (" + normalizeSQLiteCheckExpr(expr) + ")",
				IsValid:    true,
			}
		})
	}
	if len(checks) == 0 {
		return nil
	}
	return checks
}

// normalizeSQLiteCheckExpr collapses whitespace runs to single spaces so
// formatting differences between renders do not produce diff churn.
func normalizeSQLiteCheckExpr(expr string) string {
	return strings.Join(strings.Fields(expr), " ")
}

// sqliteTableBody returns the text between the outermost parentheses of a
// CREATE TABLE statement.
func sqliteTableBody(tableDDL string) (string, bool) {
	open := strings.IndexByte(tableDDL, '(')
	if open < 0 {
		return "", false
	}
	rest := tableDDL[open:]
	expr, ok := balancedParenExpr(rest)
	if !ok {
		return "", false
	}
	return expr, true
}

// balancedParenExpr returns the text between the leading '(' and its matching
// ')' (exclusive), honoring single-quote string literals. Input must start
// with '('.
func balancedParenExpr(s string) (string, bool) {
	if len(s) == 0 || s[0] != '(' {
		return "", false
	}
	count := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
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
				return s[1:i], true
			}
		}
	}
	return "", false
}

// splitSQLiteTopLevel splits a CREATE TABLE body on commas that are not nested
// in parentheses or string literals.
func splitSQLiteTopLevel(body string) []string {
	var parts []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, body[start:])
	return parts
}

// eachSQLiteCheckExpr invokes fn for every inline CHECK (<expr>) expression in
// a column definition, using a quote-aware balanced-paren scan. A name is
// reported when the check carries an inline CONSTRAINT clause.
func eachSQLiteCheckExpr(colDef string, fn func(expr string, name string)) {
	upper := strings.ToUpper(colDef)
	for i := 0; ; {
		idx := strings.Index(upper[i:], "CHECK")
		if idx < 0 {
			return
		}
		pos := i + idx + len("CHECK")
		// Skip to the opening paren, allowing whitespace only.
		for pos < len(colDef) && (colDef[pos] == ' ' || colDef[pos] == '\t' || colDef[pos] == '\n' || colDef[pos] == '\r') {
			pos++
		}
		if pos >= len(colDef) || colDef[pos] != '(' {
			i += idx + len("CHECK")
			continue
		}
		// An inline CONSTRAINT name may precede the CHECK keyword.
		before := strings.TrimRight(colDef[i:i+idx], " \t\n\r")
		name := ""
		if m := sqliteInlineNameRe.FindStringSubmatch(before); m != nil {
			name = m[1] + m[2]
		}
		expr, ok := balancedParenExpr(colDef[pos:])
		if !ok {
			return
		}
		fn(expr, name)
		i = pos + 1
	}
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
