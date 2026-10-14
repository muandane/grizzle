//go:build scenario

package scenarios

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

func getTestDSN() string {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn
	}
	return "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
}

func resetSchema(t *testing.T, db *sql.DB, schema string) {
	t.Helper()
	_, err := db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE; CREATE SCHEMA %q;", schema, schema))
	if err != nil {
		t.Fatalf("resetSchema failed: %v", err)
	}
}

// TestScenario_ParallelDeveloperRace simulates two developers (Alice and Bob)
// attempting to deploy concurrent migrations to the same database at the exact
// same millisecond.
func TestScenario_ParallelDeveloperRace(t *testing.T) {
	ctx := context.Background()
	dsn := getTestDSN()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer func() { _ = db.Close() }()

	const testSchema = "scenario_race"
	resetSchema(t, db, testSchema)

	// Step 1: Initial common baseline (users table)
	baseSchema := `CREATE TABLE users (id INT PRIMARY KEY, name TEXT);`
	baseOpts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: testSchema,
		SchemaSQL:    baseSchema,
	}
	basePlan, err := grizzle.PlanDiff(ctx, db, baseOpts)
	if err != nil {
		t.Fatalf("generating base plan: %v", err)
	}
	if err := grizzle.Apply(ctx, db, basePlan, grizzle.ApplyOpts{ExpectedHash: basePlan.Hash()}); err != nil {
		t.Fatalf("applying base plan: %v", err)
	}

	t.Log("✓ Baseline established: users table created and recorded in grizzle_history")

	// Step 2: Developer A and Developer B both generate plans against the baseline
	// Developer A wants to add an 'orders' table
	schemaA := baseSchema + "\nCREATE TABLE orders (id INT PRIMARY KEY, user_id INT, amount INT);"
	planA, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: testSchema,
		SchemaSQL:    schemaA,
	})
	if err != nil {
		t.Fatalf("plan A diff: %v", err)
	}

	// Developer B wants to add a 'payments' table
	schemaB := baseSchema + "\nCREATE TABLE payments (id INT PRIMARY KEY, user_id INT, status TEXT);"
	planB, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: testSchema,
		SchemaSQL:    schemaB,
	})
	if err != nil {
		t.Fatalf("plan B diff: %v", err)
	}

	t.Logf("✓ Plan A hash: %s (adds orders)", planA.Hash()[:16])
	t.Logf("✓ Plan B hash: %s (adds payments)", planB.Hash()[:16])

	// Step 3: Simultaneous execution barrier
	var startBarrier sync.WaitGroup
	startBarrier.Add(1)

	var doneBarrier sync.WaitGroup
	doneBarrier.Add(2)

	var errA, errB error

	go func() {
		defer doneBarrier.Done()
		startBarrier.Wait()
		// Developer A applies with pre-approved plan hash
		errA = grizzle.Apply(ctx, db, planA, grizzle.ApplyOpts{ExpectedHash: planA.Hash()})
	}()

	go func() {
		defer doneBarrier.Done()
		startBarrier.Wait()
		// Developer B applies with pre-approved plan hash
		errB = grizzle.Apply(ctx, db, planB, grizzle.ApplyOpts{ExpectedHash: planB.Hash()})
	}()

	// Fire both concurrent applies simultaneously
	startBarrier.Done()
	doneBarrier.Wait()

	t.Logf("Result A: %v", errA)
	t.Logf("Result B: %v", errB)

	// Invariant Check:
	// Exactly one developer succeeds, and the other is safely rejected with ErrPlanDrift
	// because the underlying database changed after lock acquisition.
	// ZERO dirty state, ZERO database corruption!
	var aSucceeded = errA == nil
	var bSucceeded = errB == nil

	if aSucceeded == bSucceeded {
		t.Fatalf("expected exactly one winner and one rejected, got errA=%v, errB=%v", errA, errB)
	}

	if aSucceeded {
		t.Log("✓ Developer A won the advisory lock race and applied cleanly.")
		if !errors.Is(errB, grizzle.ErrPlanDrift) {
			t.Errorf("expected Developer B to be rejected with ErrPlanDrift, got: %v", errB)
		} else {
			t.Log("✓ Developer B was safely rejected by ErrPlanDrift before any DDL executed (NO dirty state!)")
		}
	} else {
		t.Log("✓ Developer B won the advisory lock race and applied cleanly.")
		if !errors.Is(errA, grizzle.ErrPlanDrift) {
			t.Errorf("expected Developer A to be rejected with ErrPlanDrift, got: %v", errA)
		} else {
			t.Log("✓ Developer A was safely rejected by ErrPlanDrift before any DDL executed (NO dirty state!)")
		}
	}

	// Step 4: Verify the database is 100% healthy and ready
	ready, err := grizzle.IsReady(ctx, db, grizzle.WithTargetSchemas(testSchema), grizzle.WithDialect(grizzle.DialectPostgres))
	if err != nil || !ready {
		t.Fatalf("expected database to be ready, got ready=%v, err=%v", ready, err)
	}
	t.Log("✓ Database readiness verified: grizzle.IsReady = true (zero locks, zero dirty flags)")
}
