package dialect_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/dialect/sqlite"
	"github.com/muandane/grizzle/internal/plan"
	_ "modernc.org/sqlite"
)

func TestDialect_Postgres(t *testing.T) {
	p := postgres.New()
	if p.Name() != "postgres" {
		t.Errorf("expected dialect name 'postgres', got %q", p.Name())
	}

	step := plan.Step{
		SQL:   "SELECT 1;",
		NonTx: true,
	}
	stmts, err := p.Render(step)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	if len(stmts) != 1 || stmts[0].SQL != "SELECT 1;" || !stmts[0].NonTx {
		t.Errorf("unexpected Render output: %+v", stmts)
	}

	dropFK := postgres.GenerateDropFKSQL("public", "orders", "fk_orders_user")
	expected := `ALTER TABLE "public"."orders" DROP CONSTRAINT IF EXISTS "fk_orders_user";`
	if dropFK != expected {
		t.Errorf("GenerateDropFKSQL got %q, want %q", dropFK, expected)
	}
}

func TestDialect_SQLite(t *testing.T) {
	s := sqlite.New()
	if s.Name() != "sqlite" {
		t.Errorf("expected dialect name 'sqlite', got %q", s.Name())
	}

	step := plan.Step{
		SQL: "SELECT 1;",
	}
	stmts, err := s.Render(step)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	if len(stmts) != 1 || stmts[0].SQL != "SELECT 1;" || stmts[0].NonTx {
		t.Errorf("unexpected Render output: %+v", stmts)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec("CREATE TABLE test_tbl (id INTEGER PRIMARY KEY);")
	if err != nil {
		t.Fatalf("failed creating table: %v", err)
	}

	sch, err := s.Introspect(context.Background(), db, "main")
	if err != nil {
		t.Fatalf("Introspect failed: %v", err)
	}
	if sch == nil || sch.Tables["test_tbl"] == nil {
		t.Errorf("expected test_tbl in introspected schema")
	}
}
