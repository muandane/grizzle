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
		{"('draft'::text)", "'draft'"},
		{"('active'::character varying)", "'active'"},
		{"('standard')", "'standard'"},
		{"(CURRENT_TIMESTAMP)", "now()"},
		{"CURRENT_TIMESTAMP", "now()"},
		{"(now())", "now()"},
		{"now()", "now()"},
		{"true", "true"},
		{"(true)", "true"},
		{"10", "10"},
		{"(10)", "10"},
		{"", ""},
	}

	for _, tt := range tests {
		got := normalizeDefault(tt.input)
		if got != tt.expected {
			t.Errorf("normalizeDefault(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestNormalizeDefinition(t *testing.T) {
	shadow := "_grizzle_shadow"
	target := "public"

	// Index definitions
	liveIdx := "CREATE INDEX idx_orders_status ON public.orders USING btree (status) WHERE (status = 'pending'::order_status)"
	shadowIdx := "CREATE INDEX idx_orders_status ON _grizzle_shadow.orders USING btree (status) WHERE (status = 'pending'::_grizzle_shadow.order_status)"

	normLive := normalizeDefinition(liveIdx, shadow, target)
	normShadow := normalizeDefinition(shadowIdx, shadow, target)

	if normLive != normShadow {
		t.Errorf("index definitions did not normalize symmetrically:\nlive:   %q\nshadow: %q", normLive, normShadow)
	}

	// Foreign key definitions
	liveFK := "FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE"
	shadowFK := "FOREIGN KEY (customer_id) REFERENCES _grizzle_shadow.customers(id) ON DELETE CASCADE"

	normLiveFK := normalizeDefinition(liveFK, shadow, target)
	normShadowFK := normalizeDefinition(shadowFK, shadow, target)

	if normLiveFK != normShadowFK {
		t.Errorf("foreign key definitions did not normalize symmetrically:\nlive:   %q\nshadow: %q", normLiveFK, normShadowFK)
	}
}
