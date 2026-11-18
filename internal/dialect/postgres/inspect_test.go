//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestShadowInspection_UnqualifiedExpressions(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	shadowSchema := fmt.Sprintf("_grizzle_shadow_test_%d", time.Now().UnixNano()%1000000)
	targetSchema := fmt.Sprintf("target_test_%d", time.Now().UnixNano()%1000000)

	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", shadowSchema))
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", targetSchema))
	}()

	if err := postgres.SetupShadowSchema(ctx, db, shadowSchema); err != nil {
		t.Fatalf("setup shadow schema: %v", err)
	}

	schemaSQL := `
		CREATE TYPE custom_status AS ENUM ('active', 'inactive');

		CREATE FUNCTION calc_discount(price numeric) RETURNS numeric AS $$
		BEGIN
			RETURN price * 0.9;
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;

		CREATE TABLE products (
			id serial PRIMARY KEY,
			price numeric NOT NULL,
			status custom_status DEFAULT 'active',
			CONSTRAINT chk_discount CHECK (calc_discount(price) > 0)
		);

		CREATE VIEW active_products AS
		SELECT id, price, status
		FROM products
		WHERE status = 'active';
	`

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := postgres.RunShadowDDL(ctx, tx, shadowSchema, targetSchema, schemaSQL); err != nil {
		t.Fatalf("run shadow DDL: %v", err)
	}

	s, err := postgres.Inspect(ctx, tx, shadowSchema)
	if err != nil {
		t.Fatalf("inspect shadow schema: %v", err)
	}

	tbl, ok := s.Tables["products"]
	if !ok {
		t.Fatalf("table products not found in inspected shadow schema")
	}

	chk, ok := tbl.Checks["chk_discount"]
	if !ok {
		t.Fatalf("check chk_discount not found on table products")
	}

	if strings.Contains(chk.Definition, shadowSchema) {
		t.Errorf("check constraint definition contains shadow schema qualification %q: got %q", shadowSchema, chk.Definition)
	}

	view, ok := s.Views["active_products"]
	if !ok {
		t.Fatalf("view active_products not found in inspected shadow schema")
	}

	if strings.Contains(view.Definition, shadowSchema) {
		t.Errorf("view definition contains shadow schema qualification %q: got %q", shadowSchema, view.Definition)
	}
}
