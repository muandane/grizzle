package postgres_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/schema"
)

func TestGenerateCreateFunctionSQL(t *testing.T) {
	tests := []struct {
		name string
		r    *schema.Routine
		want string
	}{
		{
			name: "appends terminator",
			r:    &schema.Routine{Name: "add_one", Definition: "CREATE OR REPLACE FUNCTION public.add_one(integer) RETURNS integer LANGUAGE plpgsql AS $$BEGIN RETURN 1;$$"},
			want: "CREATE OR REPLACE FUNCTION public.add_one(integer) RETURNS integer LANGUAGE plpgsql AS $$BEGIN RETURN 1;$$;",
		},
		{
			name: "keeps existing terminator",
			r:    &schema.Routine{Name: "f", Definition: "SELECT 1;"},
			want: "SELECT 1;",
		},
		{name: "nil routine", r: nil, want: ""},
		{name: "empty definition", r: &schema.Routine{Name: "f"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := postgres.GenerateCreateFunctionSQL(tt.r); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateDropFunctionSQL(t *testing.T) {
	if got := postgres.GenerateDropFunctionSQL("public", &schema.Routine{Name: "add_one", Kind: "FUNCTION", IdentityArgs: "v integer"}); got != `DROP FUNCTION IF EXISTS "public"."add_one"(v integer);` {
		t.Fatalf("unexpected: %q", got)
	}
	if got := postgres.GenerateDropFunctionSQL("public", &schema.Routine{Name: "do_stuff", Kind: "PROCEDURE", IdentityArgs: "integer"}); got != `DROP PROCEDURE IF EXISTS "public"."do_stuff"(integer);` {
		t.Fatalf("unexpected procedure kind: %q", got)
	}
	if got := postgres.GenerateDropFunctionSQL("public", nil); got != "" {
		t.Fatalf("nil routine must render empty, got %q", got)
	}
}
