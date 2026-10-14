package lint

import (
	"fmt"
	"strings"
)

const (
	// ProfileStandard is the default rule profile containing universal correctness rules (L001..L013).
	ProfileStandard = "standard"
	// ProfileStrict enables standard rules plus high-impact production hygiene rules (L014, L022, L044, L047, L048).
	ProfileStrict = "strict"
	// ProfileAll enables all rules implemented in the engine.
	ProfileAll = "all"
)

// ExtendedRules returns the additional high-impact rules (L014, L022, L044, L047, L048).
func ExtendedRules() []Rule {
	return []Rule{
		PrimaryKeyTypeAllowlist{},
		DisallowReservedKeyword{},
		RequireBooleanDefault{},
		DisallowDuplicateIndex{},
		DisallowRedundantPrefixIndex{},
	}
}

// StrictRules returns standard rules plus extended production hygiene rules.
func StrictRules() []Rule {
	var rules []Rule
	rules = append(rules, DefaultRules()...)
	rules = append(rules, ExtendedRules()...)
	return rules
}

// AllRules returns all rules implemented in the engine.
func AllRules() []Rule {
	return StrictRules()
}

// RulesForProfile returns the rule set corresponding to a profile name.
// A blank profile name defaults to ProfileStandard.
func RulesForProfile(name string) ([]Rule, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", ProfileStandard:
		return DefaultRules(), nil
	case ProfileStrict:
		return StrictRules(), nil
	case ProfileAll:
		return AllRules(), nil
	default:
		return nil, fmt.Errorf("unknown lint profile %q; valid profiles are: %s, %s, %s",
			name, ProfileStandard, ProfileStrict, ProfileAll)
	}
}

// FilterRules excludes any rules whose ID appears in disabledIDs.
func FilterRules(rules []Rule, disabledIDs []string) []Rule {
	if len(disabledIDs) == 0 {
		return rules
	}
	disabled := make(map[string]bool, len(disabledIDs))
	for _, id := range disabledIDs {
		disabled[strings.ToUpper(strings.TrimSpace(id))] = true
	}
	var filtered []Rule
	for _, r := range rules {
		if r == nil {
			continue
		}
		if !disabled[strings.ToUpper(r.ID())] {
			filtered = append(filtered, r)
		}
	}
	return filtered
}
