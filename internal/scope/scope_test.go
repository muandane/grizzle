package scope_test

import (
	"testing"

	"github.com/yourorg/grizzle/internal/scope"
)

func TestIsTableManaged(t *testing.T) {
	filters := scope.Filters{
		Includes: []string{"users", "orders_*"},
		Excludes: []string{"orders_archive_*", "[malformed_glob"},
	}

	tests := []struct {
		table    string
		expected bool
	}{
		// Built-in extension tables and history are never managed
		{"spatial_ref_sys", false},
		{"geometry_columns", false},
		{"geography_columns", false},
		{"raster_columns", false},
		{"raster_overviews", false},
		{"grizzle_history", false},

		// Included tables
		{"users", true},
		{"orders_active", true},
		{"orders_pending", true},

		// Excluded tables overriding include
		{"orders_archive_2020", false},

		// Not in include list
		{"accounts", false},
		{"audit_log", false},

		// Malformed glob pattern literal fallback
		{"[malformed_glob", false},
	}

	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			got := scope.IsTableManaged(tt.table, filters)
			if got != tt.expected {
				t.Errorf("IsTableManaged(%q) = %v, expected %v", tt.table, got, tt.expected)
			}
		})
	}
}

func TestFilters_Validate(t *testing.T) {
	// Strict with empty includes should fail
	fStrictEmpty := scope.Filters{
		Strict:   true,
		Includes: nil,
	}
	if err := fStrictEmpty.Validate(); err == nil {
		t.Errorf("expected error for strict mode with empty includes, got nil")
	}

	// Strict with non-empty includes should pass
	fStrictNonEmpty := scope.Filters{
		Strict:   true,
		Includes: []string{"users"},
	}
	if err := fStrictNonEmpty.Validate(); err != nil {
		t.Errorf("expected nil error for strict mode with includes, got: %v", err)
	}

	// Non-strict with empty includes should pass
	fNonStrict := scope.Filters{
		Strict:   false,
		Includes: nil,
	}
	if err := fNonStrict.Validate(); err != nil {
		t.Errorf("expected nil error for non-strict mode, got: %v", err)
	}
}

func TestIsTableManaged_EmptyIncludes(t *testing.T) {
	filters := scope.Filters{
		Excludes: []string{"asynq_*"},
	}

	if !scope.IsTableManaged("users", filters) {
		t.Errorf("expected 'users' to be managed when includes are empty")
	}
	if scope.IsTableManaged("asynq_tasks", filters) {
		t.Errorf("expected 'asynq_tasks' to be excluded")
	}
}
