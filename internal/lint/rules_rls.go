package lint

import (
	"github.com/muandane/grizzle/internal/schema"
)

// RLSEnableWithoutPolicies (L008) reports tables with row-level security
// enabled but no policies declared. RLS without policies denies every
// non-owner row access (or, without FORCE, silently exempts the table
// owner), which is rarely the intended security posture.
type RLSEnableWithoutPolicies struct{}

// ID returns the rule identifier.
func (RLSEnableWithoutPolicies) ID() string { return "L008" }

// Description returns a human-readable rule summary.
func (RLSEnableWithoutPolicies) Description() string {
	return "Tables with row-level security enabled should declare at least one policy"
}

// Check returns a warning for each RLS-enabled table with zero policies.
func (RLSEnableWithoutPolicies) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, name := range sortedTableNames(s) {
		t := s.Tables[name]
		if t == nil || !t.RLSEnabled {
			continue
		}
		if len(t.Policies) > 0 {
			continue
		}
		diags = append(diags, Diagnostic{
			RuleID:   "L008",
			Severity: SeverityWarning,
			Table:    name,
			Message:  "table has row-level security enabled but no policies; every non-owner role will be denied row access",
		})
	}
	return diags
}
