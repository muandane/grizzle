package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestInvalidIndexRecovery_Postgres(t *testing.T) {
	connStr := testutil.PostgresDSN()

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres invalid index recovery test, database not reachable: %v", err)
	}

	ctx := context.Background()
	schema := fmt.Sprintf("test_idx_rec_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	schemaSQL := `
		CREATE TABLE products (
			id BIGINT PRIMARY KEY,
			sku TEXT NOT NULL
		);
		CREATE INDEX idx_products_sku ON products(sku);
	`

	// 1. Initial sync to create table and index
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    schemaSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// 2. Simulate a failed CONCURRENTLY build leaving an invalid index in Postgres catalog
	res, err := db.Exec(fmt.Sprintf(`
		UPDATE pg_index
		SET indisvalid = false
		WHERE indexrelid = '%s.idx_products_sku'::regclass;
	`, schema))
	if err != nil {
		t.Fatalf("failed marking index as invalid: %v", err)
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		t.Fatalf("no index found to mark as invalid")
	}

	// 3. PlanDiff must detect the invalid index and plan DROP INDEX CONCURRENTLY + CREATE INDEX CONCURRENTLY
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    schemaSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	if len(p.Steps) != 2 {
		t.Fatalf("expected 2 steps (DROP INDEX and CREATE INDEX), got %d steps: %+v", len(p.Steps), p.Steps)
	}
	if p.Steps[0].Type != grizzle.ChangeDropIndex {
		t.Errorf("step 0 should be DROP_INDEX, got %s", p.Steps[0].Type)
	}
	if p.Steps[1].Type != grizzle.ChangeCreateIndex {
		t.Errorf("step 1 should be CREATE_INDEX, got %s", p.Steps[1].Type)
	}

	// 4. Apply should execute repair and leave index valid
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Verify index is now valid in pg_index
	var isValid bool
	err = db.QueryRow(fmt.Sprintf(`
		SELECT ix.indisvalid
		FROM pg_index ix
		WHERE ix.indexrelid = '%s.idx_products_sku'::regclass;
	`, schema)).Scan(&isValid)
	if err != nil {
		t.Fatalf("failed querying repaired index validity: %v", err)
	}
	if !isValid {
		t.Errorf("expected index to be repaired and valid, but indisvalid is still false")
	}
}
