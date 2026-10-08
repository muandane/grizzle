package postgres_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/schema"
)

func TestGenerateCreateViewSQL(t *testing.T) {
	v := &schema.View{Name: "docs_recent", Definition: "SELECT id FROM docs WHERE created_at > now()"}
	if got, want := postgres.GenerateCreateViewSQL("public", v, false), `CREATE VIEW "public"."docs_recent" AS SELECT id FROM docs WHERE created_at > now();`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := postgres.GenerateCreateViewSQL("public", v, true), `CREATE OR REPLACE VIEW "public"."docs_recent" AS SELECT id FROM docs WHERE created_at > now();`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	mat := &schema.View{Name: "summary", IsMatView: true, Definition: "SELECT count(*) FROM docs"}
	if got, want := postgres.GenerateCreateViewSQL("public", mat, false), `CREATE MATERIALIZED VIEW "public"."summary" AS SELECT count(*) FROM docs;`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := postgres.GenerateCreateViewSQL("public", nil, false); got != "" {
		t.Fatalf("nil view must render empty, got %q", got)
	}
}

func TestGenerateDropViewSQL(t *testing.T) {
	if got, want := postgres.GenerateDropViewSQL("public", "docs_recent", false), `DROP VIEW IF EXISTS "public"."docs_recent";`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := postgres.GenerateDropViewSQL("public", "summary", true), `DROP MATERIALIZED VIEW IF EXISTS "public"."summary";`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestGenerateRefreshMatViewSQL(t *testing.T) {
	if got, want := postgres.GenerateRefreshMatViewSQL("public", "summary"), `REFRESH MATERIALIZED VIEW "public"."summary";`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
