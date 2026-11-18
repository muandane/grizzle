package postgres_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func TestGenerateCreateRangeTypeSQL(t *testing.T) {
	tests := []struct {
		name         string
		targetSchema string
		r            *schema.RangeType
		want         string
	}{
		{
			name:         "nil range type",
			targetSchema: "public",
			r:            nil,
			want:         "",
		},
		{
			name:         "minimal range type",
			targetSchema: "public",
			r: &schema.RangeType{
				Name:    "float_range",
				Subtype: "float8",
			},
			want: `CREATE TYPE "public"."float_range" AS RANGE (SUBTYPE = float8);`,
		},
		{
			name:         "full range type with all options",
			targetSchema: "public",
			r: &schema.RangeType{
				Name:           "text_range",
				Subtype:        "text",
				SubtypeOpclass: "text_ops",
				Collation:      `"POSIX"`,
				Canonical:      "text_range_canonical",
				SubtypeDiff:    "text_range_diff",
				MultirangeName: "text_multirange",
			},
			want: `CREATE TYPE "public"."text_range" AS RANGE (SUBTYPE = text, SUBTYPE_OPCLASS = text_ops, COLLATION = "POSIX", CANONICAL = text_range_canonical, SUBTYPE_DIFF = text_range_diff, MULTIRANGE_TYPE_NAME = text_multirange);`,
		},
		{
			name:         "already qualified name and empty target schema",
			targetSchema: "",
			r: &schema.RangeType{
				Name:    `"custom"."int_range"`,
				Subtype: "integer",
			},
			want: `CREATE TYPE "custom"."int_range" AS RANGE (SUBTYPE = integer);`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := postgres.GenerateCreateRangeTypeSQL(tt.targetSchema, tt.r)
			if got != tt.want {
				t.Fatalf("GenerateCreateRangeTypeSQL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateCreateCompositeTypeSQL(t *testing.T) {
	tests := []struct {
		name         string
		targetSchema string
		ct           *schema.CompositeType
		want         string
	}{
		{
			name:         "nil composite type",
			targetSchema: "public",
			ct:           nil,
			want:         "",
		},
		{
			name:         "composite type with attributes",
			targetSchema: "public",
			ct: &schema.CompositeType{
				Name: "address",
				Attributes: []*schema.CompositeAttribute{
					{Name: "street", DataType: "text"},
					{Name: "zip", DataType: "varchar(10)"},
				},
			},
			want: `CREATE TYPE "public"."address" AS ("street" text, "zip" varchar(10));`,
		},
		{
			name:         "composite type with attribute collation",
			targetSchema: "geo",
			ct: &schema.CompositeType{
				Name: "inventory_code",
				Attributes: []*schema.CompositeAttribute{
					{Name: "sku", DataType: "text", Collation: `"C"`},
					{Name: "count", DataType: "integer"},
				},
			},
			want: `CREATE TYPE "geo"."inventory_code" AS ("sku" text COLLATE "C", "count" integer);`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := postgres.GenerateCreateCompositeTypeSQL(tt.targetSchema, tt.ct)
			if got != tt.want {
				t.Fatalf("GenerateCreateCompositeTypeSQL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateAlterCompositeTypeSQL(t *testing.T) {
	tests := []struct {
		name         string
		targetSchema string
		typeName     string
		attr         *schema.CompositeAttribute
		destructive  bool
		typeChanged  bool
		want         string
	}{
		{
			name:         "nil attribute",
			targetSchema: "public",
			typeName:     "address",
			attr:         nil,
			destructive:  false,
			typeChanged:  false,
			want:         "",
		},
		{
			name:         "add attribute",
			targetSchema: "public",
			typeName:     "address",
			attr:         &schema.CompositeAttribute{Name: "country", DataType: "text"},
			destructive:  false,
			typeChanged:  false,
			want:         `ALTER TYPE "public"."address" ADD ATTRIBUTE "country" text;`,
		},
		{
			name:         "add attribute with collation",
			targetSchema: "public",
			typeName:     "address",
			attr:         &schema.CompositeAttribute{Name: "country", DataType: "text", Collation: `"en_US"`},
			destructive:  false,
			typeChanged:  false,
			want:         `ALTER TYPE "public"."address" ADD ATTRIBUTE "country" text COLLATE "en_US";`,
		},
		{
			name:         "drop attribute",
			targetSchema: "public",
			typeName:     "address",
			attr:         &schema.CompositeAttribute{Name: "zip", DataType: "varchar(10)"},
			destructive:  true,
			typeChanged:  false,
			want:         `ALTER TYPE "public"."address" DROP ATTRIBUTE "zip";`,
		},
		{
			name:         "alter attribute type",
			targetSchema: "public",
			typeName:     "address",
			attr:         &schema.CompositeAttribute{Name: "zip", DataType: "text"},
			destructive:  false,
			typeChanged:  true,
			want:         `ALTER TYPE "public"."address" ALTER ATTRIBUTE "zip" TYPE text;`,
		},
		{
			name:         "alter attribute type with collation",
			targetSchema: "public",
			typeName:     "address",
			attr:         &schema.CompositeAttribute{Name: "zip", DataType: "text", Collation: `"C"`},
			destructive:  false,
			typeChanged:  true,
			want:         `ALTER TYPE "public"."address" ALTER ATTRIBUTE "zip" TYPE text COLLATE "C";`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := postgres.GenerateAlterCompositeTypeSQL(tt.targetSchema, tt.typeName, tt.attr, tt.destructive, tt.typeChanged)
			if got != tt.want {
				t.Fatalf("GenerateAlterCompositeTypeSQL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateDropTypeSQL(t *testing.T) {
	got := postgres.GenerateDropTypeSQL("public", "old_type")
	want := `DROP TYPE IF EXISTS "public"."old_type";`
	if got != want {
		t.Fatalf("GenerateDropTypeSQL() = %q, want %q", got, want)
	}

	gotQualified := postgres.GenerateDropTypeSQL("public", `"custom"."old_type"`)
	wantQualified := `DROP TYPE IF EXISTS "custom"."old_type";`
	if gotQualified != wantQualified {
		t.Fatalf("GenerateDropTypeSQL() qualified = %q, want %q", gotQualified, wantQualified)
	}
}

func TestRenderChangeWithOpts_RangeAndCompositeTypes(t *testing.T) {
	t.Run("CreateRangeType", func(t *testing.T) {
		r := &schema.RangeType{
			Name:    "int_range",
			Subtype: "integer",
		}
		c := diff.Change{
			Type:      plan.ChangeCreateRangeType,
			Schema:    "public",
			Table:     "int_range",
			RangeType: r,
		}
		step, err := postgres.RenderChangeWithOpts("public", c, postgres.RenderOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if step.Type != plan.ChangeCreateRangeType {
			t.Errorf("step.Type = %v, want %v", step.Type, plan.ChangeCreateRangeType)
		}
		if step.SQL != `CREATE TYPE "public"."int_range" AS RANGE (SUBTYPE = integer);` {
			t.Errorf("unexpected step.SQL: %q", step.SQL)
		}
		if step.RangeType != r {
			t.Errorf("step.RangeType mismatch")
		}
	})

	t.Run("DropRangeType", func(t *testing.T) {
		c := diff.Change{
			Type:   plan.ChangeDropRangeType,
			Schema: "public",
			Table:  "int_range",
		}
		step, err := postgres.RenderChangeWithOpts("public", c, postgres.RenderOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if step.Type != plan.ChangeDropRangeType {
			t.Errorf("step.Type = %v, want %v", step.Type, plan.ChangeDropRangeType)
		}
		if step.SQL != `DROP TYPE IF EXISTS "public"."int_range";` {
			t.Errorf("unexpected step.SQL: %q", step.SQL)
		}
	})

	t.Run("CreateCompositeType", func(t *testing.T) {
		ct := &schema.CompositeType{
			Name: "coord",
			Attributes: []*schema.CompositeAttribute{
				{Name: "x", DataType: "float8"},
				{Name: "y", DataType: "float8"},
			},
		}
		c := diff.Change{
			Type:          plan.ChangeCreateCompositeType,
			Schema:        "public",
			Table:         "coord",
			CompositeType: ct,
		}
		step, err := postgres.RenderChangeWithOpts("public", c, postgres.RenderOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if step.Type != plan.ChangeCreateCompositeType {
			t.Errorf("step.Type = %v, want %v", step.Type, plan.ChangeCreateCompositeType)
		}
		if step.SQL != `CREATE TYPE "public"."coord" AS ("x" float8, "y" float8);` {
			t.Errorf("unexpected step.SQL: %q", step.SQL)
		}
		if step.CompositeType != ct {
			t.Errorf("step.CompositeType mismatch")
		}
	})

	t.Run("AlterCompositeType", func(t *testing.T) {
		attr := &schema.CompositeAttribute{Name: "z", DataType: "float8"}
		c := diff.Change{
			Type:          plan.ChangeAlterCompositeType,
			Schema:        "public",
			Table:         "coord",
			CompositeAttr: attr,
		}
		step, err := postgres.RenderChangeWithOpts("public", c, postgres.RenderOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if step.Type != plan.ChangeAlterCompositeType {
			t.Errorf("step.Type = %v, want %v", step.Type, plan.ChangeAlterCompositeType)
		}
		if step.SQL != `ALTER TYPE "public"."coord" ADD ATTRIBUTE "z" float8;` {
			t.Errorf("unexpected step.SQL: %q", step.SQL)
		}
		if step.CompositeAttr != "z" {
			t.Errorf("step.CompositeAttr = %q, want 'z'", step.CompositeAttr)
		}
	})

	t.Run("DropCompositeType", func(t *testing.T) {
		c := diff.Change{
			Type:   plan.ChangeDropCompositeType,
			Schema: "public",
			Table:  "coord",
		}
		step, err := postgres.RenderChangeWithOpts("public", c, postgres.RenderOpts{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if step.Type != plan.ChangeDropCompositeType {
			t.Errorf("step.Type = %v, want %v", step.Type, plan.ChangeDropCompositeType)
		}
		if step.SQL != `DROP TYPE IF EXISTS "public"."coord";` {
			t.Errorf("unexpected step.SQL: %q", step.SQL)
		}
	})
}
