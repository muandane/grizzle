//go:build scenario

package scenarios

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

// TestScenario_LockContentionStress simulates a high-traffic production table
// where an active client transaction holds a lock, while Grizzle runs with
// --fail-on-contention and automatically backs off and completes when the lock releases.
func TestScenario_LockContentionStress(t *testing.T) {
	ctx := context.Background()
	dsn := getTestDSN()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer func() { _ = db.Close() }()

	const testSchema = "scenario_contention"
	resetSchema(t, db, testSchema)

	// Step 1: Create initial table
	initSchema := `CREATE TABLE accounts (id INT PRIMARY KEY, balance NUMERIC(10, 2));`
	p1, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: testSchema,
		SchemaSQL:    initSchema,
	})
	if err != nil {
		t.Fatalf("diff init: %v", err)
	}
	if err := grizzle.Apply(ctx, db, p1, grizzle.ApplyOpts{}); err != nil {
		t.Fatalf("apply init: %v", err)
	}

	// Insert test row
	_, err = db.Exec(fmt.Sprintf("INSERT INTO %q.accounts VALUES (1, 1000.00);", testSchema))
	if err != nil {
		t.Fatalf("inserting test row: %v", err)
	}
	t.Log("✓ Table accounts initialized with live customer data")

	// Step 2: Separate client connection acquires an active lock on accounts
	clientDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening client db: %v", err)
	}
	defer func() { _ = clientDB.Close() }()

	clientTx, err := clientDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("client begin tx: %v", err)
	}

	// Lock the table in SHARE MODE (conflicts with ACCESS EXCLUSIVE DDL)
	_, err = clientTx.Exec(fmt.Sprintf("LOCK TABLE %q.accounts IN SHARE MODE;", testSchema))
	if err != nil {
		t.Fatalf("client lock table: %v", err)
	}
	t.Log("🔒 Background worker acquired SHARE lock on accounts")

	// Step 3: Grizzle attempts to add a column requiring strong lock with FailOnContention
	newSchema := initSchema + "\nALTER TABLE accounts ADD COLUMN status TEXT;"
	plan2, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: testSchema,
		SchemaSQL:    newSchema,
	})
	if err != nil {
		t.Fatalf("plan 2 diff: %v", err)
	}

	// Release the client lock after 600ms
	go func() {
		time.Sleep(600 * time.Millisecond)
		_ = clientTx.Commit()
		t.Log("🔓 Background worker committed and released lock on accounts")
	}()

	start := time.Now()
	err = grizzle.Apply(ctx, db, plan2, grizzle.ApplyOpts{
		FailOnContention: true,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("apply failed unexpectedly: %v", err)
	}

	if elapsed < 500*time.Millisecond {
		t.Errorf("expected migration to wait for background lock (>= 500ms), finished in %s", elapsed)
	}

	t.Logf("✓ Migration successfully backed off and applied DDL after lock cleared in %s", elapsed)
}
