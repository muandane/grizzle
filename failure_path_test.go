//go:build integration

package grizzle_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestFailurePath_StepExecutionRollbackAndHistory(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schemaName := fmt.Sprintf("test_failpath_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %q;", schemaName)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE;", schemaName))
	}()

	initialSQL := `
		CREATE TABLE customers (id BIGINT PRIMARY KEY, balance INT CHECK (balance >= 0));
	`
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaName,
		SchemaSQL:    initialSQL,
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	if _, err := db.Exec(fmt.Sprintf(`INSERT INTO %q.customers (id, balance) VALUES (1, 100);`, schemaName)); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	failingPlan := &grizzle.Plan{
		TargetSchema: schemaName,
		Steps: []grizzle.Step{
			{
				Type:  grizzle.ChangeCreateTable,
				Table: "orders",
				SQL:   fmt.Sprintf(`CREATE TABLE %q.orders (id BIGINT PRIMARY KEY);`, schemaName),
			},
			{
				Type:  grizzle.ChangeAddCheck,
				Table: "customers",
				SQL:   fmt.Sprintf(`ALTER TABLE %q.customers ADD CONSTRAINT chk_low_balance CHECK (balance < 50);`, schemaName),
			},
		},
	}

	err := grizzle.Apply(ctx, db, failingPlan, grizzle.ApplyOpts{
		ExpectedHash: failingPlan.Hash(),
	})
	if err == nil {
		t.Fatalf("expected apply to fail due to constraint violation, got nil")
	}

	var ordersExists bool
	q := `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'orders');`
	if err := db.QueryRow(q, schemaName).Scan(&ordersExists); err != nil {
		t.Fatalf("checking orders table existence failed: %v", err)
	}
	if ordersExists {
		t.Errorf("expected table 'orders' to be rolled back on step failure, but it exists")
	}

	var status string
	var failedStep int
	hq := fmt.Sprintf(`SELECT status, failed_step FROM %q.grizzle_history ORDER BY id DESC LIMIT 1;`, schemaName)
	if err := db.QueryRow(hq).Scan(&status, &failedStep); err != nil {
		t.Fatalf("querying grizzle_history failed: %v", err)
	}

	if status != "failed" {
		t.Errorf("expected history status 'failed', got %q", status)
	}
	if failedStep != 2 {
		t.Errorf("expected failed_step 2, got %d", failedStep)
	}
}
