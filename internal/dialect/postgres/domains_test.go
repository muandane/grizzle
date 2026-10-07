package postgres_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/schema"
)

func TestGenerateCreateDomainSQL(t *testing.T) {
	tests := []struct {
		name string
		d    *schema.Domain
		want string
	}{
		{
			name: "bare domain",
			d:    &schema.Domain{Name: "positive_int", BaseType: "integer", IsNullable: true},
			want: `CREATE DOMAIN "public"."positive_int" AS integer;`,
		},
		{
			name: "not null with default and check",
			d: &schema.Domain{
				Name:       "positive_int",
				BaseType:   "integer",
				IsNullable: false,
				Default:    "0",
				Checks: []*schema.CheckConstraint{
					{Name: "positive_int_check", Definition: "CHECK ((VALUE > 0))"},
				},
			},
			want: `CREATE DOMAIN "public"."positive_int" AS integer DEFAULT 0 NOT NULL CONSTRAINT "positive_int_check" CHECK ((VALUE > 0));`,
		},
		{name: "nil domain", d: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := postgres.GenerateCreateDomainSQL("public", tt.d); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateAlterDomainSQL(t *testing.T) {
	got := postgres.GenerateAlterDomainSQL("public", "positive_int", &schema.CheckConstraint{
		Name:       "positive_int_check",
		Definition: "CHECK ((VALUE > 0))",
	})
	if got != `ALTER DOMAIN "public"."positive_int" ADD CONSTRAINT "positive_int_check" CHECK ((VALUE > 0));` {
		t.Fatalf("unexpected: %q", got)
	}
	if got := postgres.GenerateAlterDomainSQL("public", "d", nil); got != "" {
		t.Fatalf("nil constraint must render empty, got %q", got)
	}
}

func TestGenerateDropDomainConstraintSQL(t *testing.T) {
	if got := postgres.GenerateDropDomainConstraintSQL("public", "positive_int", "positive_int_check"); got != `ALTER DOMAIN "public"."positive_int" DROP CONSTRAINT "positive_int_check";` {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestGenerateDropDomainSQL(t *testing.T) {
	if got := postgres.GenerateDropDomainSQL("public", "positive_int"); got != `DROP DOMAIN IF EXISTS "public"."positive_int";` {
		t.Fatalf("unexpected: %q", got)
	}
}
