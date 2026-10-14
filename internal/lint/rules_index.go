package lint

import (
	"fmt"
	"strings"

	"github.com/muandane/grizzle/internal/schema"
)

// DisallowDuplicateIndex (L047) reports indexes on the same table that cover
// the exact same column set, access method, and uniqueness. Duplicate indexes
// double disk footprint and index write maintenance cost with zero query benefit.
type DisallowDuplicateIndex struct{}

// ID returns the rule identifier.
func (DisallowDuplicateIndex) ID() string { return "L047" }

// Description returns a human-readable rule summary.
func (DisallowDuplicateIndex) Description() string {
	return "A table must not declare duplicate indexes covering the identical columns and predicate"
}

// Check returns an error diagnostic for duplicate index definitions.
func (DisallowDuplicateIndex) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, tableName := range sortedTableNames(s) {
		t := s.Tables[tableName]
		if t == nil {
			continue
		}
		seen := make(map[string]string) // signature -> first index name
		for _, idxName := range sortedIndexNames(t) {
			idx := t.Indexes[idxName]
			if idx == nil {
				continue
			}
			cols := indexColumns(idx.Definition)
			if len(cols) == 0 {
				continue
			}
			sig := fmt.Sprintf("%s|%t|%s", strings.Join(cols, ","), idx.IsUnique, idx.Predicate)
			if prev, exists := seen[sig]; exists {
				diags = append(diags, Diagnostic{
					RuleID:   "L047",
					Severity: SeverityError,
					Table:    tableName,
					Column:   idxName,
					Message:  "duplicate index " + idxName + ": covers the same columns as " + prev,
				})
			} else {
				seen[sig] = idxName
			}
		}
	}
	return diags
}

// DisallowRedundantPrefixIndex (L048) reports standalone single-column non-unique
// indexes that are rendered redundant by an existing composite index whose
// leading prefix covers the exact same column.
type DisallowRedundantPrefixIndex struct{}

// ID returns the rule identifier.
func (DisallowRedundantPrefixIndex) ID() string { return "L048" }

// Description returns a human-readable rule summary.
func (DisallowRedundantPrefixIndex) Description() string {
	return "Single-column index is redundant when a composite index covers the column as its leading prefix"
}

// Check returns a warning diagnostic for redundant prefix indexes.
func (DisallowRedundantPrefixIndex) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, tableName := range sortedTableNames(s) {
		t := s.Tables[tableName]
		if t == nil {
			continue
		}

		type idxInfo struct {
			name      string
			cols      []string
			isUnique  bool
			isPartial bool
		}

		var indexes []idxInfo
		for _, idxName := range sortedIndexNames(t) {
			idx := t.Indexes[idxName]
			if idx == nil {
				continue
			}
			cols := indexColumns(idx.Definition)
			if len(cols) > 0 {
				indexes = append(indexes, idxInfo{
					name:      idxName,
					cols:      cols,
					isUnique:  idx.IsUnique,
					isPartial: idx.IsPartial(),
				})
			}
		}

		// Check each single-column non-unique non-partial index against composite indexes
		for _, a := range indexes {
			if len(a.cols) != 1 || a.isUnique || a.isPartial {
				continue
			}
			singleCol := a.cols[0]
			for _, b := range indexes {
				if len(b.cols) > 1 && !b.isPartial && b.cols[0] == singleCol {
					diags = append(diags, Diagnostic{
						RuleID:   "L048",
						Severity: SeverityWarning,
						Table:    tableName,
						Column:   a.name,
						Message:  "index " + a.name + " on (" + singleCol + ") is redundant; composite index " + b.name + " already covers it as a leading prefix",
					})
					break // report once per redundant index
				}
			}
		}
	}
	return diags
}
