package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

func TestSimulate_SQLite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	opts := grizzle.Options{
		Dialect:         grizzle.DialectSQLite,
		SchemaSQL:       `CREATE TABLE notes (id INTEGER PRIMARY KEY, content TEXT NOT NULL);`,
		Simulate:        true,
		MaxLockDuration: 100 * time.Millisecond,
	}

	res, err := grizzle.Simulate(ctx, db, opts)
	if err != nil {
		t.Fatalf("Simulate failed: %v", err)
	}

	if res == nil || res.Plan == nil {
		t.Fatal("expected non-nil SimulateResult with Plan")
	}
	if len(res.StepTimings) == 0 {
		t.Fatal("expected steps in simulation")
	}

	// Verify table was not persisted
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='notes'`).Scan(&count); err != nil {
		t.Fatalf("querying table: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected table notes to not exist after simulation, found %d", count)
	}
}

func TestSimulatePlan_SQLite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	schema := `CREATE TABLE todos (id INTEGER PRIMARY KEY, title TEXT);`
	opts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schema,
	}

	p, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Happy path SimulatePlan
	res, err := grizzle.SimulatePlan(ctx, db, p, opts)
	if err != nil {
		t.Fatalf("SimulatePlan failed: %v", err)
	}
	if len(res.StepTimings) == 0 {
		t.Fatal("expected step timings in SimulatePlan result")
	}

	// Hash mismatch check
	badOpts := opts
	badOpts.ExpectedHash = "invalidhash12345"
	_, err = grizzle.SimulatePlan(ctx, db, p, badOpts)
	if !errors.Is(err, grizzle.ErrPlanDrift) {
		t.Fatalf("expected ErrPlanDrift for wrong ExpectedHash, got: %v", err)
	}

	// Nil plan check
	_, err = grizzle.SimulatePlan(ctx, db, nil, opts)
	if err == nil {
		t.Fatal("expected error on nil plan")
	}
}

func TestOptions_WithSimulateAndMaxLockDuration(t *testing.T) {
	opts := grizzle.Options{}
	grizzle.WithSimulate(true)(&opts)
	if !opts.Simulate {
		t.Errorf("expected Simulate to be true")
	}
	grizzle.WithMaxLockDuration(250 * time.Millisecond)(&opts)
	if opts.MaxLockDuration != 250*time.Millisecond {
		t.Errorf("expected MaxLockDuration to be 250ms, got %v", opts.MaxLockDuration)
	}
}
