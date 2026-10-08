package grizzle_test

import (
	"errors"
	"testing"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/dialect/sqlite"
	_ "modernc.org/sqlite"
)

// v1: named table-level CHECK plus an inline column-level CHECK.
const checkSchemaV1 = `
	CREATE TABLE products (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		price NUMERIC NOT NULL CONSTRAINT products_price_positive CHECK (price > 0),
		stock INTEGER NOT NULL DEFAULT 0 CHECK (stock >= 0)
	);
`

// v2 adds a new named CHECK constraint.
const checkSchemaV2 = `
	CREATE TABLE products (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		price NUMERIC NOT NULL CONSTRAINT products_price_positive CHECK (price > 0),
		stock INTEGER NOT NULL DEFAULT 0 CHECK (stock >= 0),
		CONSTRAINT products_name_nonempty CHECK (length(name) > 0)
	);
`

// v3 drops the inline column-level CHECK.
const checkSchemaV3 = `
	CREATE TABLE products (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		price NUMERIC NOT NULL CONSTRAINT products_price_positive CHECK (price > 0),
		stock INTEGER NOT NULL DEFAULT 0,
		CONSTRAINT products_name_nonempty CHECK (length(name) > 0)
	);
`

func TestSQLite_CheckLifecycle(t *testing.T) {
	db := getSQLiteDB(t)
	ctx := t.Context()

	opts := func(schemaSQL string) grizzle.Options {
		return grizzle.Options{
			Dialect:      grizzle.DialectSQLite,
			TargetSchema: "main",
			SchemaSQL:    schemaSQL,
		}
	}

	// 1. Greenfield sync: named + inline CHECKs are created and introspected.
	if err := grizzle.Sync(ctx, db, opts(checkSchemaV1)); err != nil {
		t.Fatalf("greenfield sync: %v", err)
	}
	live, err := sqlite.Inspect(ctx, db)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	tbl := live.Tables["products"]
	if tbl == nil {
		t.Fatalf("products table missing")
	}
	if len(tbl.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %+v", tbl.Checks)
	}
	price, ok := tbl.Checks["products_price_positive"]
	if !ok || price.Definition != "CHECK (price > 0)" {
		t.Fatalf("named check missing or wrong definition: %+v", price)
	}
	inline, ok := tbl.Checks["products_stock_check"]
	if !ok || inline.Definition != "CHECK (stock >= 0)" {
		t.Fatalf("inline check missing or wrong definition: %+v", inline)
	}

	// 2. Second sync: no-op (rebuild must reproduce CHECK constraints).
	p, err := grizzle.PlanDiff(ctx, db, opts(checkSchemaV1))
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("second sync must be a no-op, got %+v", p.Steps)
	}

	// 3. Adding a named CHECK: rebuild, non-destructive (ADD_CHECK).
	p2, err := grizzle.PlanDiff(ctx, db, opts(checkSchemaV2))
	if err != nil {
		t.Fatalf("add plan: %v", err)
	}
	if len(p2.Steps) != 1 || p2.Steps[0].Type != grizzle.ChangeAddCheck || p2.Steps[0].Destructive {
		t.Fatalf("check addition must be a single non-destructive ADD_CHECK rebuild, got %+v", p2.Steps)
	}
	if err := grizzle.Sync(ctx, db, opts(checkSchemaV2)); err != nil {
		t.Fatalf("add sync: %v", err)
	}
	live, err = sqlite.Inspect(ctx, db)
	if err != nil {
		t.Fatalf("inspect after add: %v", err)
	}
	if _, ok := live.Tables["products"].Checks["products_name_nonempty"]; !ok {
		t.Fatalf("added check missing: %+v", live.Tables["products"].Checks)
	}

	// 4. Dropping a CHECK: rebuild is destructive and gated by AllowCheck.
	p3, err := grizzle.PlanDiff(ctx, db, opts(checkSchemaV3))
	if err != nil {
		t.Fatalf("drop plan: %v", err)
	}
	if len(p3.Steps) != 1 || p3.Steps[0].Type != grizzle.ChangeDropCheck || !p3.Steps[0].Destructive {
		t.Fatalf("check removal must be a single destructive DROP_CHECK rebuild, got %+v", p3.Steps)
	}
	if _, _, _, blocked := p3.Summary(); blocked != 1 {
		t.Fatalf("expected 1 blocked step with default policy, got blocked=%d", blocked)
	}
	if err := grizzle.Sync(ctx, db, opts(checkSchemaV3)); !errors.Is(err, grizzle.ErrHazardBlocked) && err == nil {
		t.Fatalf("drop sync must fail with default policy, got %v", err)
	}

	// 5. Allowed drop proceeds; data is preserved across the rebuild.
	if _, err := db.Exec(`INSERT INTO products (id, name, price, stock) VALUES (1, 'widget', 9.99, 5);`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	dropOpts := opts(checkSchemaV3)
	dropOpts.AllowDropCheck = new(true)
	dropOpts.AcceptHazards = []grizzle.HazardCode{grizzle.HazardDropCheck}
	if err := grizzle.Sync(ctx, db, dropOpts); err != nil {
		t.Fatalf("allowed drop sync: %v", err)
	}
	live, err = sqlite.Inspect(ctx, db)
	if err != nil {
		t.Fatalf("inspect after drop: %v", err)
	}
	if _, ok := live.Tables["products"].Checks["products_stock_check"]; ok {
		t.Fatalf("dropped check still present: %+v", live.Tables["products"].Checks)
	}
	if len(live.Tables["products"].Checks) != 2 {
		t.Fatalf("remaining checks wrong: %+v", live.Tables["products"].Checks)
	}
	var price2 float64
	var name2 string
	if err := db.QueryRow(`SELECT name, price FROM products WHERE id = 1;`).Scan(&name2, &price2); err != nil {
		t.Fatalf("row must survive rebuild: %v", err)
	}
	if name2 != "widget" || price2 != 9.99 {
		t.Fatalf("row data corrupted across rebuild: %s %f", name2, price2)
	}

	// 6. Final no-op.
	p4, err := grizzle.PlanDiff(ctx, db, opts(checkSchemaV3))
	if err != nil {
		t.Fatalf("final plan: %v", err)
	}
	if len(p4.Steps) != 0 {
		t.Fatalf("final sync must be a no-op, got %+v", p4.Steps)
	}
}
