//go:build integration

package exec_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestSimulatePostgres_ZeroMutationAndProfiling(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := fmt.Sprintf("test_sim_%d", time.Now().UnixNano())
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE %s.users (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email TEXT NOT NULL
		);
		INSERT INTO %s.users (email) VALUES ('alice@example.com'), ('bob@example.com');
	`, schema, schema)); err != nil {
		t.Fatalf("seeding live schema: %v", err)
	}

	desired := `
		CREATE TABLE users (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email TEXT NOT NULL
		);
		CREATE TABLE orders (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			user_id INTEGER REFERENCES users (id),
			amount NUMERIC(10,2) NOT NULL
		);
		CREATE INDEX idx_orders_user_id ON orders (user_id);
	`

	before, err := postgres.Inspect(ctx, db, schema)
	if err != nil {
		t.Fatalf("inspecting before: %v", err)
	}

	metrics := &simMockMetrics{}
	res, err := exec.SimulatePostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL:    desired,
		Metrics:      metrics,
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("SimulatePostgres failed: %v", err)
	}

	if res == nil {
		t.Fatal("expected non-nil SimulateResult")
	}
	if len(res.StepTimings) == 0 {
		t.Fatal("expected StepTimings to be populated")
	}
	if res.TotalDuration <= 0 {
		t.Errorf("expected positive TotalDuration, got %v", res.TotalDuration)
	}
	for i, st := range res.StepTimings {
		if st.Duration <= 0 && !st.Step.NonTx {
			t.Errorf("step %d: expected positive duration, got %v", i+1, st.Duration)
		}
		if st.ExceededLockMax {
			t.Errorf("step %d: unexpectedly exceeded threshold", i+1)
		}
	}
	if len(res.ExceededWarnings) > 0 {
		t.Errorf("unexpected warnings: %v", res.ExceededWarnings)
	}

	// Verify catalog is completely untouched
	after, err := postgres.Inspect(ctx, db, schema)
	if err != nil {
		t.Fatalf("inspecting after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("schema catalog changed after simulation:\nbefore: %+v\nafter:  %+v", before, after)
	}

	// Verify data untouched
	var count int
	if err := db.QueryRow(fmt.Sprintf("SELECT count(*) FROM %s.users", schema)).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 user rows, got %d", count)
	}

	// Verify metrics
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if len(metrics.simulations) != 1 {
		t.Fatalf("expected 1 simulation metric call, got %d", len(metrics.simulations))
	}
	if metrics.simulations[0].dialect != "postgres" || metrics.simulations[0].status != "applied" {
		t.Errorf("unexpected metric record: %+v", metrics.simulations[0])
	}
}

func TestSimulatePostgres_ThresholdWarning(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := fmt.Sprintf("test_sim_warn_%d", time.Now().UnixNano())
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE %s.articles (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, body TEXT);
	`, schema)); err != nil {
		t.Fatalf("seeding schema: %v", err)
	}

	desired := `
		CREATE TABLE articles (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, body TEXT, title TEXT);
	`

	// Threshold set to 1 nanosecond
	res, err := exec.SimulatePostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL:    desired,
	}, 1*time.Nanosecond)
	if err != nil {
		t.Fatalf("SimulatePostgres failed: %v", err)
	}

	hasExceeded := false
	for _, st := range res.StepTimings {
		if st.ExceededLockMax {
			hasExceeded = true
			break
		}
	}
	if !hasExceeded {
		t.Error("expected at least one step timing to exceed threshold")
	}
	if len(res.ExceededWarnings) == 0 {
		t.Error("expected ExceededWarnings when threshold is exceeded")
	}
}

func TestSimulatePostgres_ConstraintViolation_Rollback(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := fmt.Sprintf("test_sim_fail_%d", time.Now().UnixNano())
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE %s.products (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, price INT NOT NULL);
		INSERT INTO %s.products (price) VALUES (-10), (50);
	`, schema, schema)); err != nil {
		t.Fatalf("seeding schema: %v", err)
	}

	before, err := postgres.Inspect(ctx, db, schema)
	if err != nil {
		t.Fatalf("inspecting before: %v", err)
	}

	desired := `
		CREATE TABLE products (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			price INT NOT NULL,
			CONSTRAINT chk_pos_price CHECK (price > 0)
		);
	`

	metrics := &simMockMetrics{}
	_, err = exec.SimulatePostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL:    desired,
		Metrics:      metrics,
	}, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected simulation to fail on CHECK constraint violation against existing data")
	}
	if !strings.Contains(err.Error(), "simulation failed at step") {
		t.Errorf("unexpected error format: %v", err)
	}

	after, err := postgres.Inspect(ctx, db, schema)
	if err != nil {
		t.Fatalf("inspecting after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("schema catalog changed after failed simulation")
	}

	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if len(metrics.simulations) != 1 || metrics.simulations[0].status != "failed" {
		t.Errorf("expected 1 metric with status 'failed', got %+v", metrics.simulations)
	}
}
