package schema_test

import (
	"strings"
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

func TestNormalizeDefinitionWithMappings_MultiSchema(t *testing.T) {
	mappings := map[string]string{
		"_grizzle_shadow_identity": "identity",
		"_grizzle_shadow_billing":  "billing",
		"_grizzle_shadow_public":   "public",
	}

	// 1. Cross-schema foreign key: table in billing referencing table in identity
	shadowFK := "FOREIGN KEY (user_id) REFERENCES _grizzle_shadow_identity.users(id) ON DELETE CASCADE"
	liveFK := "FOREIGN KEY (user_id) REFERENCES identity.users(id) ON DELETE CASCADE"

	normShadowFK := schema.NormalizeDefinitionWithMappings(shadowFK, mappings, "billing")
	normLiveFK := schema.NormalizeDefinitionWithMappings(liveFK, mappings, "billing")

	if normShadowFK != normLiveFK {
		t.Fatalf("cross-schema foreign keys did not normalize symmetrically:\nshadow: %q\nlive:   %q", normShadowFK, normLiveFK)
	}
	if normShadowFK != liveFK {
		t.Errorf("expected cross-schema reference to identity.users to be preserved: got %q, want %q", normShadowFK, liveFK)
	}

	// 2. Intra-schema foreign key: table in billing referencing table in billing
	shadowIntraFK := "FOREIGN KEY (account_id) REFERENCES _grizzle_shadow_billing.accounts(id)"
	liveIntraFK := "FOREIGN KEY (account_id) REFERENCES accounts(id)"

	normShadowIntra := schema.NormalizeDefinitionWithMappings(shadowIntraFK, mappings, "billing")
	normLiveIntra := schema.NormalizeDefinitionWithMappings(liveIntraFK, mappings, "billing")

	if normShadowIntra != normLiveIntra {
		t.Fatalf("intra-schema foreign keys did not normalize symmetrically:\nshadow: %q\nlive:   %q", normShadowIntra, normLiveIntra)
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

func TestNormalizeCheckDef(t *testing.T) {
	tests := []struct {
		name     string
		live     string
		desired  string
		expected string
	}{
		{
			name:     "extra outer parentheses and numeric cast",
			live:     "CHECK ((price > (0)::numeric))",
			desired:  "CHECK (price > 0)",
			expected: "CHECK (price > 0)",
		},
		{
			name:     "multiple parens and text cast",
			live:     "CHECK (((status)::text = 'active'::character varying))",
			desired:  "CHECK (status = 'active')",
			expected: "CHECK (status = 'active')",
		},
		{
			name:     "not valid preservation",
			live:     "CHECK ((amount >= 100)) NOT VALID",
			desired:  "CHECK (amount >= 100) NOT VALID",
			expected: "CHECK (amount >= 100) NOT VALID",
		},
		{
			name:     "schema unmapping in check",
			live:     "CHECK (public.valid_range(start_val, end_val))",
			desired:  "CHECK (_grizzle_shadow.valid_range(start_val, end_val))",
			expected: "CHECK (valid_range(start_val, end_val))",
		},
	}

	mappings := map[string]string{"_grizzle_shadow": "public"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normLive := schema.NormalizeCheckDef(tt.live, mappings, "public")
			normDesired := schema.NormalizeCheckDef(tt.desired, mappings, "public")
			if normLive != normDesired {
				t.Errorf("symmetrical normalization failed:\nlive:    %q\ndesired: %q", normLive, normDesired)
			}
			if normLive != tt.expected {
				t.Errorf("expected canonical %q, got %q", tt.expected, normLive)
			}
		})
	}
}

func TestNormalizePolicyExpr(t *testing.T) {
	tests := []struct {
		name     string
		live     string
		desired  string
		expected string
	}{
		{
			name:     "extra outer parens",
			live:     "((user_id = auth.uid()))",
			desired:  "user_id = auth.uid()",
			expected: "user_id = auth.uid()",
		},
		{
			name:     "text cast on string literal and redundant whitespace",
			live:     "((tenant_id = 'acme'::text))",
			desired:  "tenant_id  =  'acme'",
			expected: "tenant_id = 'acme'",
		},
		{
			name:     "column cast on varchar column",
			live:     "((role)::text = 'admin')",
			desired:  "role = 'admin'",
			expected: "role = 'admin'",
		},
		{
			name:     "boolean comparison",
			live:     "(is_active = true)",
			desired:  "is_active = true",
			expected: "is_active = true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normLive := schema.NormalizePolicyExpr(tt.live, nil)
			normDesired := schema.NormalizePolicyExpr(tt.desired, nil)
			if normLive != normDesired {
				t.Errorf("policy expr normalization failed:\nlive:    %q\ndesired: %q", normLive, normDesired)
			}
			if normLive != tt.expected {
				t.Errorf("expected canonical %q, got %q", tt.expected, normLive)
			}
		})
	}
}

func TestNormalizeGeneratedExpr(t *testing.T) {
	tests := []struct {
		name     string
		live     string
		desired  string
		expected string
	}{
		{
			name:     "outer parens and numeric literal cast",
			live:     "(price * (0.1)::numeric)",
			desired:  "price * 0.1",
			expected: "price * 0.1",
		},
		{
			name:     "column cast in function call",
			live:     "(lower((email)::text))",
			desired:  "lower(email)",
			expected: "lower(email)",
		},
		{
			name:     "simple addition without redundant casts",
			live:     "((a + b))",
			desired:  "a + b",
			expected: "a + b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normLive := schema.NormalizeGeneratedExpr(tt.live)
			normDesired := schema.NormalizeGeneratedExpr(tt.desired)
			if normLive != normDesired {
				t.Errorf("generated expr normalization failed:\nlive:    %q\ndesired: %q", normLive, normDesired)
			}
			if normLive != tt.expected {
				t.Errorf("expected canonical %q, got %q", tt.expected, normLive)
			}
		})
	}
}

func TestNormalizeIndexPredicates(t *testing.T) {
	shadow := "_grizzle_shadow"
	target := "public"

	// Predicates with parenthesis and whitespace drift
	liveIdx := "CREATE INDEX idx_active ON public.users USING btree (created_at) WHERE (active IS TRUE)"
	shadowIdx := "CREATE INDEX idx_active ON _grizzle_shadow.users (created_at) WHERE (active IS TRUE)"

	normLive := schema.NormalizeDefinition(liveIdx, shadow, target)
	normShadow := schema.NormalizeDefinition(shadowIdx, shadow, target)

	if normLive != normShadow {
		t.Errorf("index definitions with boolean predicate drift did not normalize symmetrically:\nlive:   %q\nshadow: %q", normLive, normShadow)
	}

	// Functional index expression casts ((lower((email)::text))) vs (lower(email))
	liveFuncIdx := "CREATE INDEX idx_email ON public.users USING btree ((lower((email)::text)))"
	shadowFuncIdx := "CREATE INDEX idx_email ON _grizzle_shadow.users ((lower(email)))"

	normLiveFunc := schema.NormalizeDefinition(liveFuncIdx, shadow, target)
	normShadowFunc := schema.NormalizeDefinition(shadowFuncIdx, shadow, target)

	if normLiveFunc != normShadowFunc {
		t.Errorf("functional index definitions did not normalize symmetrically:\nlive:   %q\nshadow: %q", normLiveFunc, normShadowFunc)
	}
}

func TestNormalize_ThreeValuedLogic(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		forbidden string
		expected  string
	}{
		{
			name:      "is not true preserved",
			input:     "val IS NOT TRUE",
			forbidden: "!= true",
			expected:  "val IS NOT TRUE",
		},
		{
			name:      "is not false preserved",
			input:     "val IS NOT FALSE",
			forbidden: "!= false",
			expected:  "val IS NOT FALSE",
		},
		{
			name:      "is true preserved",
			input:     "val IS TRUE",
			forbidden: "= true",
			expected:  "val IS TRUE",
		},
		{
			name:      "is false preserved",
			input:     "val IS FALSE",
			forbidden: "= false",
			expected:  "val IS FALSE",
		},
		{
			name:      "mixed case is not true preserved",
			input:     "val is not true",
			forbidden: "!= true",
			expected:  "val is not true",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := schema.NormalizeExpression(tc.input, nil)
			if strings.Contains(got, tc.forbidden) {
				t.Fatalf("NormalizeExpression(%q) produced forbidden %q: got %q", tc.input, tc.forbidden, got)
			}
			if got != tc.expected {
				t.Errorf("NormalizeExpression(%q) = %q, expected %q", tc.input, got, tc.expected)
			}
		})
	}
}
