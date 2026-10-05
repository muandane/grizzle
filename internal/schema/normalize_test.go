package schema_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

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
		got := schema.NormalizeType(tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeType(%q) = %q, expected %q", tt.input, got, tt.expected)
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
		{"('READY)'::text)", "'READY)'"},
		{"('READY('::text)", "'READY('"},
		{"('^(a|b)$'::text)", "'^(a|b)$'"},
		{"(CURRENT_TIMESTAMP)", "now()"},
		{"CURRENT_TIMESTAMP", "now()"},
		{"(now())", "now()"},
		{"now()", "now()"},
		{"true", "true"},
		{"(true)", "true"},
		{"10", "10"},
		{"(10)", "10"},
		{"(-5)", "-5"},
		// Compound expressions must NOT have their trailing casts stripped or be corrupted
		{"('2024-01-01'::date + '1 day'::interval)", "('2024-01-01'::date + '1 day'::interval)"},
		{"('a'::text || 'b'::text)", "('a'::text || 'b'::text)"},
		{"", ""},
	}

	for _, tt := range tests {
		got := schema.NormalizeDefault(tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeDefault(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestNormalizeDefinition(t *testing.T) {
	shadow := "_grizzle_shadow"
	target := "public"

	// Index definitions
	liveIdx := "CREATE INDEX idx_orders_status ON public.orders USING btree (status) WHERE (status = 'pending'::order_status)"
	shadowIdx := "CREATE INDEX idx_orders_status ON _grizzle_shadow.orders USING btree (status) WHERE (status = 'pending'::_grizzle_shadow.order_status)"

	normLive := schema.NormalizeDefinition(liveIdx, shadow, target)
	normShadow := schema.NormalizeDefinition(shadowIdx, shadow, target)

	if normLive != normShadow {
		t.Errorf("index definitions did not normalize symmetrically:\nlive:   %q\nshadow: %q", normLive, normShadow)
	}

	// Foreign key definitions
	liveFK := "FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE"
	shadowFK := "FOREIGN KEY (customer_id) REFERENCES _grizzle_shadow.customers(id) ON DELETE CASCADE"

	normLiveFK := schema.NormalizeDefinition(liveFK, shadow, target)
	normShadowFK := schema.NormalizeDefinition(shadowFK, shadow, target)

	if normLiveFK != normShadowFK {
		t.Errorf("foreign key definitions did not normalize symmetrically:\nlive:   %q\nshadow: %q", normLiveFK, normShadowFK)
	}

	// Predicate with string literal matching "ON public." must not be corrupted
	predIdx := "CREATE INDEX idx_flag ON public.orders (status) WHERE (status = 'ON public.flag')"
	normPred := schema.NormalizeDefinition(predIdx, shadow, target)
	expectedPred := "CREATE INDEX idx_flag ON orders (status) WHERE (status = 'ON public.flag')"
	if normPred != expectedPred {
		t.Errorf("predicate with literal 'ON public.flag' was mutated:\ngot:  %q\nwant: %q", normPred, expectedPred)
	}
}

func TestIsTypeNarrowing(t *testing.T) {
	tests := []struct {
		oldType  string
		newType  string
		expected bool
	}{
		// Identical
		{"bigint", "bigint", false},
		{"integer", "int4", false},
		{"varchar(255)", "character varying(255)", false},

		// Integers
		{"bigint", "integer", true},
		{"bigint", "smallint", true},
		{"integer", "smallint", true},
		{"integer", "bigint", false},
		{"smallint", "integer", false},
		{"smallint", "bigint", false},

		// Floats
		{"double precision", "real", true},
		{"real", "double precision", false},

		// Integers & Floats
		{"integer", "double precision", false},
		{"real", "integer", true},

		// Strings
		{"text", "varchar(255)", true},
		{"varchar(255)", "text", false},
		{"varchar(100)", "varchar(50)", true},
		{"varchar(50)", "varchar(100)", false},
		{"varchar", "varchar(100)", true},
		{"varchar(100)", "varchar", false},
		{"char(50)", "char(20)", true},
		{"char(20)", "char(50)", false},

		// Scalar -> String (safe)
		{"integer", "text", false},
		{"bigint", "varchar(255)", false},

		// Numerics
		{"numeric(10,2)", "numeric(8,2)", true},
		{"numeric(10,2)", "numeric(10,1)", true},
		{"numeric(8,2)", "numeric(10,2)", false},
		{"numeric", "numeric(10,2)", true},
		{"numeric(10,2)", "numeric", false},
		{"integer", "numeric", false},

		// Incompatible / cross-domain (narrowing/risky)
		{"text", "integer", true},
		{"timestamp", "date", true},
		{"boolean", "integer", true},
	}

	for _, tt := range tests {
		got := schema.IsTypeNarrowing(tt.oldType, tt.newType)
		if got != tt.expected {
			t.Errorf("IsTypeNarrowing(%q, %q) = %v, expected %v", tt.oldType, tt.newType, got, tt.expected)
		}
	}
}

