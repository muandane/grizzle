package lint

import (
	"regexp"
	"strings"
)

var (
	// Matches "-- grizzle:lint-ignore L001, L002" or "-- grizzle:lint-disable L001"
	suppressDirectiveRe = regexp.MustCompile(`(?i)--\s*grizzle:lint-(?:ignore|disable)\s+([^\r\n]+)`)
	tableDeclRe         = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:[a-zA-Z0-9_"]+\.)?["']?([a-zA-Z0-9_]+)["']?`)
)

type suppressions struct {
	global map[string]bool
	table  map[string]map[string]bool // tableName (lowercased) -> ruleID (uppercased) -> true
}

func parseSuppressions(sqlText string) suppressions {
	s := suppressions{
		global: make(map[string]bool),
		table:  make(map[string]map[string]bool),
	}
	if strings.TrimSpace(sqlText) == "" {
		return s
	}

	lines := strings.Split(sqlText, "\n")
	var pendingRules []string
	currentTable := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Check directive
		if m := suppressDirectiveRe.FindStringSubmatch(trimmed); m != nil {
			rulesPart := m[1]
			parts := strings.FieldsFunc(rulesPart, func(r rune) bool {
				return r == ',' || r == ' ' || r == '\t'
			})
			isGlobal := strings.Contains(strings.ToLower(trimmed), "lint-disable")
			for _, p := range parts {
				p = strings.TrimSpace(strings.ToUpper(p))
				if p != "" {
					if isGlobal {
						s.global[p] = true
					} else {
						pendingRules = append(pendingRules, p)
					}
				}
			}
			continue
		}

		// Check table declaration
		if tm := tableDeclRe.FindStringSubmatch(trimmed); tm != nil {
			currentTable = strings.ToLower(tm[1])
			if s.table[currentTable] == nil {
				s.table[currentTable] = make(map[string]bool)
			}
			for _, r := range pendingRules {
				s.table[currentTable][r] = true
			}
			pendingRules = nil
			continue
		}

		// If pendingRules exist and we hit a non-comment non-empty line that isn't CREATE TABLE
		if len(pendingRules) > 0 && trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			if currentTable != "" {
				if s.table[currentTable] == nil {
					s.table[currentTable] = make(map[string]bool)
				}
				for _, r := range pendingRules {
					s.table[currentTable][r] = true
				}
			} else {
				for _, r := range pendingRules {
					s.global[r] = true
				}
			}
			pendingRules = nil
		}

		// End of table statement
		if currentTable != "" && strings.Contains(trimmed, ");") {
			currentTable = ""
		}
	}

	// Any leftover pending rules at EOF are global
	for _, r := range pendingRules {
		s.global[r] = true
	}

	return s
}

// FilterSuppressed filters out diagnostics matching inline suppressions in sourceSQL.
func FilterSuppressed(sourceSQL string, diags []Diagnostic) []Diagnostic {
	if len(diags) == 0 || strings.TrimSpace(sourceSQL) == "" {
		return diags
	}

	sup := parseSuppressions(sourceSQL)
	if len(sup.global) == 0 && len(sup.table) == 0 {
		return diags
	}

	var kept []Diagnostic
	for _, d := range diags {
		ruleUpper := strings.ToUpper(d.RuleID)
		if sup.global[ruleUpper] {
			continue
		}
		tblLower := strings.ToLower(d.Table)
		if tblRules, ok := sup.table[tblLower]; ok && tblRules[ruleUpper] {
			continue
		}
		kept = append(kept, d)
	}
	return kept
}
