// Package lint provides static, database-free linting of a desired schema IR.
//
// It is a pure layer: it must never import database/sql, context, os, net, or
// slog, and only depends on internal/schema. Rules operate on the compiled
// *schema.Schema intermediate representation produced by shadow compilation,
// so diagnostics are 100% deterministic for identical input DDL.
package lint

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/muandane/grizzle/internal/schema"
)

// Severity indicates how strongly a diagnostic should block a release.
type Severity string

const (
	// SeverityError marks a structural anti-pattern that must be fixed.
	SeverityError Severity = "ERROR"
	// SeverityWarning marks a recommendation that may be ignored deliberately.
	SeverityWarning Severity = "WARNING"
	// SeverityInfo marks a stylistic suggestion with no correctness impact.
	SeverityInfo Severity = "INFO"
)

// Diagnostic is a single lint finding against a schema element.
type Diagnostic struct {
	RuleID   string   `json:"rule_id"`
	Severity Severity `json:"severity"`
	Table    string   `json:"table"`
	Column   string   `json:"column,omitempty"`
	Message  string   `json:"message"`
	Line     int      `json:"line,omitempty"`
}

// Rule is a pure check executed against the desired schema IR.
type Rule interface {
	ID() string
	Description() string
	Check(s *schema.Schema) []Diagnostic
}

// Lint runs the provided rules against the schema and returns diagnostics
// sorted deterministically by rule ID, table, column, then message.
func Lint(s *schema.Schema, rules ...Rule) []Diagnostic {
	var diags []Diagnostic
	for _, r := range rules {
		if r == nil {
			continue
		}
		diags = append(diags, r.Check(s)...)
	}
	sortDiagnostics(diags)
	return diags
}

// DefaultRules returns the built-in rule set (L001..L009).
func DefaultRules() []Rule {
	return []Rule{
		MissingPrimaryKey{},
		UnindexedForeignKey{},
		NamingConvention{},
		PreferIdentityOverSerial{},
		CheckNamingConvention{},
		DuplicateCheckConstraint{},
		PreferNamedChecks{},
		RLSEnableWithoutPolicies{},
		NoDMLStatements{},
	}
}

// HasErrors reports whether any diagnostic has SeverityError.
func HasErrors(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

func sortDiagnostics(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		a, b := diags[i], diags[j]
		switch {
		case a.RuleID != b.RuleID:
			return a.RuleID < b.RuleID
		case a.Table != b.Table:
			return a.Table < b.Table
		case a.Column != b.Column:
			return a.Column < b.Column
		default:
			return a.Message < b.Message
		}
	})
}

// FormatText renders diagnostics as human-readable lines.
func FormatText(w io.Writer, diags []Diagnostic) error {
	for _, d := range diags {
		target := d.Table
		if d.Column != "" {
			target = fmt.Sprintf("%s.%s", d.Table, d.Column)
		}
		if _, err := fmt.Fprintf(w, "%s [%s] %s: %s\n", d.RuleID, d.Severity, target, d.Message); err != nil {
			return err
		}
	}
	summary := fmt.Sprintf("%d diagnostic(s)", len(diags))
	if HasErrors(diags) {
		summary += fmt.Sprintf(", %d error(s)", countSeverity(diags, SeverityError))
	}
	if n := countSeverity(diags, SeverityWarning); n > 0 {
		summary += fmt.Sprintf(", %d warning(s)", n)
	}
	if n := countSeverity(diags, SeverityInfo); n > 0 {
		summary += fmt.Sprintf(", %d info(s)", n)
	}
	_, err := fmt.Fprintln(w, summary)
	return err
}

// FormatJSON renders diagnostics as a JSON array.
func FormatJSON(w io.Writer, diags []Diagnostic) error {
	if diags == nil {
		diags = []Diagnostic{}
	}
	data, err := json.MarshalIndent(diags, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// FormatGitHub renders diagnostics as GitHub Actions workflow annotations.
// INFO findings map to the "notice" annotation level; ERROR and WARNING map
// to the "error" and "warning" levels respectively.
func FormatGitHub(w io.Writer, diags []Diagnostic) error {
	for _, d := range diags {
		target := d.Table
		if d.Column != "" {
			target = fmt.Sprintf("%s.%s", d.Table, d.Column)
		}
		level := strings.ToLower(string(d.Severity))
		if d.Severity == SeverityInfo {
			level = "notice"
		}
		if _, err := fmt.Fprintf(w, "::%s title=%s::%s: %s\n",
			level, d.RuleID, target, d.Message); err != nil {
			return err
		}
	}
	return nil
}

func countSeverity(diags []Diagnostic, sev Severity) int {
	n := 0
	for _, d := range diags {
		if d.Severity == sev {
			n++
		}
	}
	return n
}
