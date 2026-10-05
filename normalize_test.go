package grizzle

import "testing"

func TestNormalizeType(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"int4", "integer"},
		{"int", "integer"},
		{"serial", "integer"},
		{"int8", "bigint"},
		{"bigserial", "bigint"},
		{"bool", "boolean"},
		{"character varying(255)", "varchar(255)"},
		{"character varying", "varchar"},
		{"timestamp with time zone", "timestamptz"},
		{"timestamp without time zone", "timestamp"},
		{"float8", "double precision"},
		{"jsonb", "jsonb"},
		{"uuid", "uuid"},
	}

	for _, tt := range tests {
		got := normalizeType(tt.input)
		if got != tt.expected {
			t.Errorf("normalizeType(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestNormalizeDefault(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"'active'::character varying", "'active'"},
		{"'pending'::text", "'pending'"},
		{"CURRENT_TIMESTAMP", "now()"},
		{"now()", "now()"},
		{"true", "true"},
		{"10", "10"},
		{"", ""},
	}

	for _, tt := range tests {
		got := normalizeDefault(tt.input)
		if got != tt.expected {
			t.Errorf("normalizeDefault(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}
