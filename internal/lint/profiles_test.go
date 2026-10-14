package lint

import (
	"slices"
	"testing"
)

func TestRulesForProfile(t *testing.T) {
	// Standard profile (default)
	std, err := RulesForProfile("standard")
	if err != nil {
		t.Fatalf("unexpected error for standard profile: %v", err)
	}
	if len(std) != 13 {
		t.Errorf("expected 13 standard rules, got %d", len(std))
	}

	// Blank defaults to standard
	blank, err := RulesForProfile("")
	if err != nil {
		t.Fatalf("unexpected error for blank profile: %v", err)
	}
	if len(blank) != len(std) {
		t.Errorf("blank profile should match standard rules count")
	}

	// Strict profile
	strict, err := RulesForProfile("strict")
	if err != nil {
		t.Fatalf("unexpected error for strict profile: %v", err)
	}
	if len(strict) != 18 { // 13 standard + 5 extended
		t.Errorf("expected 18 strict rules, got %d", len(strict))
	}

	// Unknown profile returns error
	_, err = RulesForProfile("nonexistent")
	if err == nil {
		t.Errorf("expected error for nonexistent profile, got nil")
	}
}

func TestFilterRules(t *testing.T) {
	all := DefaultRules()
	filtered := FilterRules(all, []string{"l001", "L003"})

	var ids []string
	for _, r := range filtered {
		ids = append(ids, r.ID())
	}

	if slices.Contains(ids, "L001") || slices.Contains(ids, "L003") {
		t.Errorf("filtered rules should not contain L001 or L003, got: %v", ids)
	}
	if len(filtered) != len(all)-2 {
		t.Errorf("expected %d rules after filtering, got %d", len(all)-2, len(filtered))
	}
}
