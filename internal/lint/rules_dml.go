package lint

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/muandane/grizzle/internal/schema"
)

// dmlKindRe extracts the leading DML keyword from a statement for message
// reporting; it mirrors schema.FindDMLStatements' accepted prefixes.
var dmlKindRe = regexp.MustCompile(`(?is)^\s*(INSERT|UPDATE|DELETE|TRUNCATE)\b`)

// NoDMLStatements (L009) reports DML statements (INSERT/UPDATE/DELETE/
// TRUNCATE) declared in SchemaSQL. The desired schema must be data-free:
// DML in SchemaSQL is silently ignored by shadow compilation (it never
// re-executes against the target), so seeds silently fail to land. Seed
// data belongs in SeedSQL, which is hash-gated and applied idempotently.
type NoDMLStatements struct{}

// ID returns the rule identifier.
func (NoDMLStatements) ID() string { return "L009" }

// Description returns a human-readable rule summary.
func (NoDMLStatements) Description() string {
	return "SchemaSQL must not contain DML (INSERT/UPDATE/DELETE/TRUNCATE); use SeedSQL for data"
}

// Check returns an error for each DML statement found in the raw SchemaSQL
// the IR was compiled from.
func (NoDMLStatements) Check(s *schema.Schema) []Diagnostic {
	var diags []Diagnostic
	for _, stmt := range schema.FindDMLStatements(s.SourceSQL) {
		kind := "DML"
		if m := dmlKindRe.FindStringSubmatch(stmt); m != nil {
			kind = strings.ToUpper(m[1])
		}
		diags = append(diags, Diagnostic{
			RuleID:   "L009",
			Severity: SeverityError,
			Message: fmt.Sprintf("%s statement in SchemaSQL is silently ignored by automigrations; move seed data to SeedSQL: %.80s",
				kind, strings.TrimSpace(stmt)),
		})
	}
	return diags
}
