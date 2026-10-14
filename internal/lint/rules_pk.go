package lint

import (
	"strings"

	"github.com/muandane/grizzle/internal/schema"
)

// allowedPKTypes contains standard approved data types for primary keys.
var allowedPKTypes = map[string]bool{
	"bigint":      true,
	"int8":        true,
	"bigserial":   true,
	"integer":     true,
	"int":         true,
	"int4":        true,
	"serial":      true,
	"smallint":    true,
	"int2":        true,
	"smallserial": true,
	"uuid":        true,
}

// PrimaryKeyTypeAllowlist (L014) reports primary key columns that use
// non-standard or dangerous data types (e.g. VARCHAR, TEXT, FLOAT, JSON).
// Primary keys should be compact, immutable integers or UUIDs to maintain
// optimal b-tree depth, foreign key join performance, and row stability.
type PrimaryKeyTypeAllowlist struct{}

// ID returns the rule identifier.
func (PrimaryKeyTypeAllowlist) ID() string { return "L014" }

// Description returns a human-readable rule summary.
func (PrimaryKeyTypeAllowlist) Description() string {
	return "Primary key columns must use an allowed integer or UUID data type (bigint, integer, smallint, uuid)"
}

// Check returns an error diagnostic for each primary key column whose type
// is not in the approved allowlist. Partitions are skipped because parent keys cover them.
func (PrimaryKeyTypeAllowlist) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, tableName := range sortedTableNames(s) {
		t := s.Tables[tableName]
		if t == nil || t.IsPartition() || t.PrimaryKey == nil {
			continue
		}
		for _, colName := range t.PrimaryKey.Columns {
			c := t.Columns[colName]
			if c == nil {
				continue
			}
			baseType := strings.ToLower(strings.TrimSpace(c.DataType))
			if idx := strings.Index(baseType, "("); idx > 0 {
				baseType = strings.TrimSpace(baseType[:idx])
			}
			if !allowedPKTypes[baseType] {
				diags = append(diags, Diagnostic{
					RuleID:   "L014",
					Severity: SeverityError,
					Table:    tableName,
					Column:   colName,
					Message:  "primary key column " + colName + " uses type " + c.DataType + "; prefer BIGINT, INTEGER, or UUID",
				})
			}
		}
	}
	return diags
}
