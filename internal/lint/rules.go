package lint

import (
	"regexp"
	"sort"
	"strings"

	"github.com/muandane/grizzle/internal/schema"
)

// snakeCase matches lowercase snake_case identifiers.
var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// fkColumnsRe locates the column list of a FOREIGN KEY constraint definition
// as produced by pg_get_constraintdef, e.g.
// "FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE".
var fkColumnsRe = regexp.MustCompile(`(?i)FOREIGN\s+KEY\s*\(([^()]*)\)`)

// MissingPrimaryKey (L001) reports tables without a primary key. Tables without
// primary keys break logical replication in PostgreSQL (REPLICA IDENTITY
// DEFAULT), prevent unique row identification, and degrade performance.
type MissingPrimaryKey struct{}

// ID returns the rule identifier.
func (MissingPrimaryKey) ID() string { return "L001" }

// Description returns a human-readable rule summary.
func (MissingPrimaryKey) Description() string {
	return "Every managed table should declare an explicit primary key"
}

// Check returns a diagnostic for each table missing a primary key.
// Partitions are skipped because the parent partitioned table's primary key
// covers them.
func (MissingPrimaryKey) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, name := range sortedTableNames(s) {
		t := s.Tables[name]
		if t == nil || t.IsPartition() {
			continue
		}
		if t.PrimaryKey == nil || len(t.PrimaryKey.Columns) == 0 {
			diags = append(diags, Diagnostic{
				RuleID:   "L001",
				Severity: SeverityError,
				Table:    name,
				Message:  "table has no primary key",
			})
		}
	}
	return diags
}

// UnindexedForeignKey (L002) reports foreign key columns not covered by any
// index. When a parent row is updated or deleted, PostgreSQL must check the
// child table; without an index on the foreign key columns that check becomes
// a full sequential scan of the child table, causing severe latency and
// deadlocks under concurrent writes.
type UnindexedForeignKey struct{}

// ID returns the rule identifier.
func (UnindexedForeignKey) ID() string { return "L002" }

// Description returns a human-readable rule summary.
func (UnindexedForeignKey) Description() string {
	return "Foreign key columns should be covered by an index"
}

// Check returns a warning for each foreign key whose column set is not a
// prefix of any table index (the primary key counts as an index).
func (UnindexedForeignKey) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, name := range sortedTableNames(s) {
		t := s.Tables[name]
		if t == nil {
			continue
		}
		for _, fkName := range sortedFKNames(t) {
			fk := t.ForeignKeys[fkName]
			if fk == nil {
				continue
			}
			cols := foreignKeyColumns(fk.Definition)
			if len(cols) == 0 {
				continue // unparseable definition: skip rather than false-positive
			}
			if coveredByPrefixIndex(pkColumns(t), cols) {
				continue
			}
			covered := false
			for _, idxName := range sortedIndexNames(t) {
				idx := t.Indexes[idxName]
				if idx == nil {
					continue
				}
				if coveredByPrefixIndex(indexColumns(idx.Definition), cols) {
					covered = true
					break
				}
			}
			if covered {
				continue
			}
			diags = append(diags, Diagnostic{
				RuleID:   "L002",
				Severity: SeverityWarning,
				Table:    name,
				Column:   strings.Join(cols, ", "),
				Message:  "foreign key columns are not covered by any index; parent-table changes will sequential-scan this table",
			})
		}
	}
	return diags
}

// NamingConvention (L003) reports identifiers that are not lowercase
// snake_case. Mixed-case identifiers require quoting in PostgreSQL and cause
// subtle bugs in cross-dialect tooling.
type NamingConvention struct{}

// ID returns the rule identifier.
func (NamingConvention) ID() string { return "L003" }

// Description returns a human-readable rule summary.
func (NamingConvention) Description() string {
	return "Identifiers should use lowercase snake_case (^[a-z][a-z0-9_]*$)"
}

// Check validates table, column, index, and enum names.
func (NamingConvention) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, name := range sortedTableNames(s) {
		t := s.Tables[name]
		if t == nil {
			continue
		}
		diags = append(diags, namingDiag("table", name, name)...)
		for _, colName := range sortedColumnNames(t) {
			diags = append(diags, namingDiag("column", colName, t.Name)...)
		}
		for _, idxName := range sortedIndexNames(t) {
			if idx := t.Indexes[idxName]; idx != nil {
				diags = append(diags, namingDiag("index", idxName, t.Name)...)
			}
		}
	}
	for _, enumName := range sortedEnumNames(s) {
		diags = append(diags, namingDiag("enum", enumName, enumName)...)
	}
	return diags
}

func namingDiag(kind, ident, table string) []Diagnostic {
	if snakeCase.MatchString(ident) {
		return nil
	}
	return []Diagnostic{{
		RuleID:   "L003",
		Severity: SeverityWarning,
		Table:    table,
		Column:   ident,
		Message:  kind + " name " + ident + " is not lowercase snake_case",
	}}
}

// PreferIdentityOverSerial (L004) reports columns relying on legacy SERIAL
// pseudo-types (detected as a nextval(...) default) instead of the
// SQL-standard GENERATED ALWAYS AS IDENTITY (PostgreSQL 10+). Identity
// columns conform to standard SQL and prevent sequence ownership leaks.
type PreferIdentityOverSerial struct{}

// ID returns the rule identifier.
func (PreferIdentityOverSerial) ID() string { return "L004" }

// Description returns a human-readable rule summary.
func (PreferIdentityOverSerial) Description() string {
	return "Prefer GENERATED ALWAYS AS IDENTITY over SERIAL / nextval defaults"
}

// Check flags columns with a nextval(...) default that are not declared
// identity columns. SQLite has no sequences, so it never triggers there.
func (PreferIdentityOverSerial) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, name := range sortedTableNames(s) {
		t := s.Tables[name]
		if t == nil {
			continue
		}
		for _, colName := range sortedColumnNames(t) {
			c := t.Columns[colName]
			if c == nil || c.IsIdentity {
				continue
			}
			isSerial := strings.Contains(strings.ToLower(c.DataType), "serial") ||
				strings.Contains(c.DefaultValue, "nextval(")
			if !isSerial {
				continue
			}
			diags = append(diags, Diagnostic{
				RuleID:   "L004",
				Severity: SeverityWarning,
				Table:    name,
				Column:   colName,
				Message:  "column uses a legacy SERIAL/nextval default; prefer GENERATED ALWAYS AS IDENTITY",
			})
		}
	}
	return diags
}

// pkColumns returns the primary key columns of a table, if any.
func pkColumns(t *schema.Table) []string {
	if t.PrimaryKey == nil {
		return nil
	}
	return t.PrimaryKey.Columns
}

// coveredByPrefixIndex reports whether cols form an order-insensitive subset
// of the leading len(cols) entries of indexCols.
func coveredByPrefixIndex(indexCols, cols []string) bool {
	if len(indexCols) < len(cols) || len(cols) == 0 {
		return false
	}
	remaining := make(map[string]int, len(cols))
	for _, c := range cols {
		remaining[c]++
	}
	for _, ic := range indexCols[:len(cols)] {
		if n, ok := remaining[ic]; ok {
			remaining[ic] = n - 1
			if n-1 == 0 {
				delete(remaining, ic)
			}
		}
	}
	return len(remaining) == 0
}

// foreignKeyColumns extracts the local column list from a constraint
// definition, normalizing quotes and whitespace.
func foreignKeyColumns(def string) []string {
	m := fkColumnsRe.FindStringSubmatch(def)
	if m == nil {
		return nil
	}
	return splitColumns(m[1])
}

// indexColumns extracts the column list from a pg_get_indexdef-style index
// definition (the first balanced parenthesized group), e.g.
// "CREATE INDEX idx ON public.users USING btree (user_id)".
func indexColumns(def string) []string {
	start := strings.Index(def, "(")
	if start < 0 {
		return nil
	}
	depth := 0
	for i := start; i < len(def); i++ {
		switch def[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return splitColumns(def[start+1 : i])
			}
		}
	}
	return nil
}

// splitColumns splits a comma-separated column list at top-level nesting,
// trimming whitespace and surrounding quotes.
func splitColumns(list string) []string {
	var cols []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		c := strings.TrimSpace(cur.String())
		c = strings.Trim(c, `"`)
		if c != "" {
			cols = append(cols, c)
		}
		cur.Reset()
	}
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '(':
			depth++
			cur.WriteByte('(')
		case ')':
			depth--
			cur.WriteByte(')')
		case ',':
			if depth == 0 {
				flush()
			} else {
				cur.WriteByte(',')
			}
		default:
			cur.WriteByte(list[i])
		}
	}
	flush()
	return cols
}

func sortedTableNames(s *schema.Schema) []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.Tables))
	for n := range s.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortedColumnNames(t *schema.Table) []string {
	names := make([]string, 0, len(t.Columns))
	for n := range t.Columns {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortedIndexNames(t *schema.Table) []string {
	names := make([]string, 0, len(t.Indexes))
	for n := range t.Indexes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortedFKNames(t *schema.Table) []string {
	names := make([]string, 0, len(t.ForeignKeys))
	for n := range t.ForeignKeys {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortedEnumNames(s *schema.Schema) []string {
	names := make([]string, 0, len(s.Enums))
	for n := range s.Enums {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
