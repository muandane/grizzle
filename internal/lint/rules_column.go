package lint

import (
	"strings"

	"github.com/muandane/grizzle/internal/schema"
)

// RequireBooleanDefault (L044) reports boolean columns that do not declare
// an explicit DEFAULT value. In SQL, boolean columns without a default evaluate
// to NULL when omitted, creating dangerous tri-state logic bugs (TRUE, FALSE, NULL)
// that lead to unexpected conditional evaluation failures.
type RequireBooleanDefault struct{}

// ID returns the rule identifier.
func (RequireBooleanDefault) ID() string { return "L044" }

// Description returns a human-readable rule summary.
func (RequireBooleanDefault) Description() string {
	return "Boolean columns should declare an explicit DEFAULT false or DEFAULT true to avoid tri-state NULL"
}

// Check returns a warning diagnostic for each boolean column lacking a default expression.
func (RequireBooleanDefault) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, tableName := range sortedTableNames(s) {
		t := s.Tables[tableName]
		if t == nil {
			continue
		}
		for _, colName := range sortedColumnNames(t) {
			c := t.Columns[colName]
			if c == nil {
				continue
			}
			dataType := strings.ToLower(strings.TrimSpace(c.DataType))
			if dataType != "boolean" && dataType != "bool" {
				continue
			}
			if strings.TrimSpace(c.DefaultValue) == "" {
				diags = append(diags, Diagnostic{
					RuleID:   "L044",
					Severity: SeverityWarning,
					Table:    tableName,
					Column:   colName,
					Message:  "boolean column " + colName + " has no DEFAULT; prefer DEFAULT false or DEFAULT true to prevent tri-state NULL",
				})
			}
		}
	}
	return diags
}
