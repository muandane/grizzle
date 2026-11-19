package lint

import (
	"fmt"
	"strings"

	"github.com/muandane/grizzle/internal/schema"
)

var financialSubstrings = []string{"amount", "price", "balance", "cost", "total", "fee"}

func isFinancialColumnName(name string) bool {
	lower := strings.ToLower(name)
	for _, sub := range financialSubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func isFloatingPointType(dt string) bool {
	u := strings.ToUpper(strings.TrimSpace(dt))
	switch u {
	case "FLOAT", "REAL", "FLOAT4", "FLOAT8", "DOUBLE PRECISION":
		return true
	}
	return strings.HasPrefix(u, "FLOAT(")
}

// DisallowFloatForFinancialData (L032) flags columns with names indicative of financial
// data (e.g. amount, price, balance, cost, total, fee) that use IEEE 754 floating-point
// data types instead of arbitrary-precision NUMERIC or DECIMAL.
type DisallowFloatForFinancialData struct{}

// ID returns the rule identifier.
func (DisallowFloatForFinancialData) ID() string { return "L032" }

// Description returns a human-readable rule summary.
func (DisallowFloatForFinancialData) Description() string {
	return "Disallow floating-point types for monetary/financial data; use NUMERIC or DECIMAL"
}

// Check returns an error diagnostic for each financial column declared with floating-point types.
func (DisallowFloatForFinancialData) Check(s *schema.Schema) []Diagnostic {
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
			if isFinancialColumnName(colName) && isFloatingPointType(c.DataType) {
				diags = append(diags, Diagnostic{
					RuleID:   "L032",
					Severity: SeverityError,
					Table:    tableName,
					Column:   colName,
					Message:  fmt.Sprintf("column %q has floating-point type %q for financial data; use NUMERIC or DECIMAL", colName, c.DataType),
				})
			}
		}
	}
	return diags
}

// RequireTableComment (L016) flags tables that lack an informative COMMENT ON TABLE description.
// Table comments ensure continuous schema documentation in production catalogs.
// This rule is skipped for SQLite schemas as SQLite does not support table comments.
type RequireTableComment struct{}

// ID returns the rule identifier.
func (RequireTableComment) ID() string { return "L016" }

// Description returns a human-readable rule summary.
func (RequireTableComment) Description() string {
	return "Tables should declare a COMMENT ON TABLE description"
}

// Check returns an info diagnostic for each table missing a comment description.
func (RequireTableComment) Check(s *schema.Schema) []Diagnostic {
	if s != nil && strings.EqualFold(s.Dialect, "sqlite") {
		return nil
	}
	var diags []Diagnostic
	for _, tableName := range sortedTableNames(s) {
		t := s.Tables[tableName]
		if t == nil || t.IsPartition() {
			continue
		}
		if strings.TrimSpace(t.Comment) == "" {
			diags = append(diags, Diagnostic{
				RuleID:   "L016",
				Severity: SeverityInfo,
				Table:    tableName,
				Message:  fmt.Sprintf("table %q has no COMMENT ON TABLE description", tableName),
			})
		}
	}
	return diags
}

var tenantIsolationColumns = map[string]bool{
	"tenant_id":       true,
	"org_id":          true,
	"organization_id": true,
	"account_id":      true,
}

// DisallowCascadeDeleteOnMultiTenant (L033) flags foreign keys using ON DELETE CASCADE
// on multi-tenant isolation columns (tenant_id, org_id, organization_id, account_id).
// Accidental parent tenant deletions must not cascade to child tenant data.
type DisallowCascadeDeleteOnMultiTenant struct{}

// ID returns the rule identifier.
func (DisallowCascadeDeleteOnMultiTenant) ID() string { return "L033" }

// Description returns a human-readable rule summary.
func (DisallowCascadeDeleteOnMultiTenant) Description() string {
	return "Foreign keys on multi-tenant isolation columns should not use ON DELETE CASCADE"
}

// Check returns a warning diagnostic for foreign keys referencing tenant isolation columns with ON DELETE CASCADE.
func (DisallowCascadeDeleteOnMultiTenant) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, tableName := range sortedTableNames(s) {
		t := s.Tables[tableName]
		if t == nil {
			continue
		}
		for _, fkName := range sortedFKNames(t) {
			fk := t.ForeignKeys[fkName]
			if fk == nil {
				continue
			}
			if !strings.Contains(strings.ToUpper(fk.Definition), "ON DELETE CASCADE") {
				continue
			}
			cols := foreignKeyColumns(fk.Definition)
			hasTenantCol := false
			for _, col := range cols {
				if tenantIsolationColumns[strings.ToLower(col)] {
					hasTenantCol = true
					break
				}
			}
			if hasTenantCol {
				diags = append(diags, Diagnostic{
					RuleID:   "L033",
					Severity: SeverityWarning,
					Table:    tableName,
					Column:   fkName,
					Message:  fmt.Sprintf("foreign key %q uses ON DELETE CASCADE on multi-tenant isolation column; prefer ON DELETE RESTRICT", fkName),
				})
			}
		}
	}
	return diags
}

// CompositeIndexMaxColumns (L049) flags composite indexes spanning more than 4 columns.
// Excessively wide indexes bloat the B-tree, increase write latency, and offer diminishing returns.
type CompositeIndexMaxColumns struct{}

// ID returns the rule identifier.
func (CompositeIndexMaxColumns) ID() string { return "L049" }

// Description returns a human-readable rule summary.
func (CompositeIndexMaxColumns) Description() string {
	return "Composite indexes should not exceed 4 columns to avoid index bloat"
}

// Check returns a warning diagnostic for composite indexes spanning > 4 columns.
func (CompositeIndexMaxColumns) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, tableName := range sortedTableNames(s) {
		t := s.Tables[tableName]
		if t == nil {
			continue
		}
		for _, idxName := range sortedIndexNames(t) {
			idx := t.Indexes[idxName]
			if idx == nil {
				continue
			}
			cols := indexColumns(idx.Definition)
			if len(cols) > 4 {
				diags = append(diags, Diagnostic{
					RuleID:   "L049",
					Severity: SeverityWarning,
					Table:    tableName,
					Column:   idxName,
					Message:  fmt.Sprintf("index %q spans %d columns (maximum recommended is 4); risk of index bloat", idxName, len(cols)),
				})
			}
		}
	}
	return diags
}

var lowCardinalityBooleanColNames = map[string]bool{
	"is_active":  true,
	"is_deleted": true,
	"deleted":    true,
	"enabled":    true,
	"active":     true,
}

// DisallowLowCardinalityIndexPrefix (L051) flags composite indexes whose leading column
// has low cardinality (e.g. boolean flags or boolean column names). Low-cardinality prefixes
// rarely filter rows effectively; higher-cardinality columns should be placed first or
// converted to a partial index.
type DisallowLowCardinalityIndexPrefix struct{}

// ID returns the rule identifier.
func (DisallowLowCardinalityIndexPrefix) ID() string { return "L051" }

// Description returns a human-readable rule summary.
func (DisallowLowCardinalityIndexPrefix) Description() string {
	return "Composite indexes should not use a low-cardinality boolean column as the leading prefix"
}

// Check returns a warning diagnostic for composite indexes with a leading boolean column.
func (DisallowLowCardinalityIndexPrefix) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, tableName := range sortedTableNames(s) {
		t := s.Tables[tableName]
		if t == nil {
			continue
		}
		for _, idxName := range sortedIndexNames(t) {
			idx := t.Indexes[idxName]
			if idx == nil {
				continue
			}
			cols := indexColumns(idx.Definition)
			if len(cols) < 2 {
				continue
			}
			lead := cols[0]
			fields := strings.Fields(lead)
			if len(fields) == 0 {
				continue
			}
			leadColName := strings.Trim(fields[0], `"`)

			isBool := lowCardinalityBooleanColNames[strings.ToLower(leadColName)]
			if !isBool && t.Columns != nil {
				if col, ok := t.Columns[leadColName]; ok && col != nil {
					dt := strings.ToLower(strings.TrimSpace(col.DataType))
					if dt == "boolean" || dt == "bool" {
						isBool = true
					}
				} else {
					for _, col := range t.Columns {
						if col != nil && strings.EqualFold(col.Name, leadColName) {
							dt := strings.ToLower(strings.TrimSpace(col.DataType))
							if dt == "boolean" || dt == "bool" {
								isBool = true
							}
							break
						}
					}
				}
			}

			if isBool {
				diags = append(diags, Diagnostic{
					RuleID:   "L051",
					Severity: SeverityWarning,
					Table:    tableName,
					Column:   idxName,
					Message:  fmt.Sprintf("composite index %q has low-cardinality leading boolean column %q; reorder higher-cardinality columns first or convert to partial index", idxName, leadColName),
				})
			}
		}
	}
	return diags
}
