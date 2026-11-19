package exec_test

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	_ "modernc.org/sqlite"
)

type simMockMetrics struct {
	mu          sync.Mutex
	simulations []struct {
		duration time.Duration
		dialect  string
		status   string
	}
}

func (m *simMockMetrics) RecordMigrationDuration(ctx context.Context, duration time.Duration, dialect string, status string) {
}

func (m *simMockMetrics) RecordDriftDetected(ctx context.Context, dialect string, schema string) {
}

func (m *simMockMetrics) RecordLockWait(ctx context.Context, duration time.Duration, dialect string) {
}

func (m *simMockMetrics) RecordSimulationDuration(ctx context.Context, duration time.Duration, dialect string, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.simulations = append(m.simulations, struct {
		duration time.Duration
		dialect  string
		status   string
	}{duration, dialect, status})
}

func takeSQLiteSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY name`)
	if err != nil {
		t.Fatalf("snapshotting sqlite: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var sb strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning sqlite snapshot: %v", err)
		}
		sb.WriteString(s)
		sb.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating sqlite snapshot: %v", err)
	}
	return sb.String()
}

func TestSimulateSQLite_Success_ZeroMutation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, price INTEGER NOT NULL);`); err != nil {
		t.Fatalf("seeding table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO orders (price) VALUES (10), (20);`); err != nil {
		t.Fatalf("seeding rows: %v", err)
	}

	beforeSnapshot := takeSQLiteSnapshot(t, db)

	desired := `
		CREATE TABLE orders (id INTEGER PRIMARY KEY, price INTEGER NOT NULL);
		CREATE INDEX idx_orders_price ON orders (price);
		CREATE TABLE shipments (id INTEGER PRIMARY KEY, order_id INTEGER REFERENCES orders (id));
	`

	metrics := &simMockMetrics{}
	res, err := exec.SimulateSQLite(ctx, db, exec.SQLiteExecConfig{
		SchemaSQL: desired,
		Metrics:   metrics,
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("SimulateSQLite failed: %v", err)
	}

	if res == nil {
		t.Fatal("expected non-nil SimulateResult")
	}
	if res.Plan == nil {
		t.Fatal("expected non-nil Plan in SimulateResult")
	}
	if res.TotalDuration <= 0 {
		t.Errorf("expected positive TotalDuration, got %v", res.TotalDuration)
	}
	if len(res.StepTimings) == 0 {
		t.Fatal("expected StepTimings to be populated")
	}
	for i, st := range res.StepTimings {
		if st.Duration <= 0 && !st.Step.NonTx {
			t.Errorf("step %d: expected positive duration, got %v", i+1, st.Duration)
		}
		if st.ExceededLockMax {
			t.Errorf("step %d unexpectedly marked as ExceededLockMax", i+1)
		}
	}
	if len(res.ExceededWarnings) > 0 {
		t.Errorf("unexpected warnings: %v", res.ExceededWarnings)
	}

	// Verify zero schema mutations
	afterSnapshot := takeSQLiteSnapshot(t, db)
	if beforeSnapshot != afterSnapshot {
		t.Fatalf("schema catalog changed after simulation:\nbefore: %s\nafter:  %s", beforeSnapshot, afterSnapshot)
	}

	// Verify shipments table does not exist
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='shipments';`).Scan(&count); err != nil {
		t.Fatalf("querying sqlite_master: %v", err)
	}
	if count != 0 {
		t.Errorf("table shipments should not exist after simulation, found %d", count)
	}

	// Verify row data intact
	var rowCount int
	if err := db.QueryRow(`SELECT count(*) FROM orders;`).Scan(&rowCount); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if rowCount != 2 {
		t.Errorf("expected 2 rows in orders, got %d", rowCount)
	}

	// Verify metrics
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if len(metrics.simulations) != 1 {
		t.Fatalf("expected 1 simulation metric call, got %d", len(metrics.simulations))
	}
	if metrics.simulations[0].dialect != "sqlite" || metrics.simulations[0].status != "applied" {
		t.Errorf("unexpected metric record: %+v", metrics.simulations[0])
	}
}

func TestSimulateSQLite_ThresholdWarnings(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);`); err != nil {
		t.Fatalf("seeding table: %v", err)
	}

	desired := `
		CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, email TEXT);
		CREATE TABLE profiles (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users (id));
	`

	// Max lock duration threshold set to 1 nanosecond; real execution exceeds this.
	res, err := exec.SimulateSQLite(ctx, db, exec.SQLiteExecConfig{
		SchemaSQL: desired,
	}, 1*time.Nanosecond)
	if err != nil {
		t.Fatalf("SimulateSQLite failed: %v", err)
	}

	if len(res.StepTimings) == 0 {
		t.Fatal("expected StepTimings to be populated")
	}

	hasExceeded := false
	for _, st := range res.StepTimings {
		if st.ExceededLockMax {
			hasExceeded = true
			break
		}
	}
	if !hasExceeded {
		t.Error("expected at least one step timing to exceed 1ns threshold")
	}

	if len(res.ExceededWarnings) == 0 {
		t.Fatal("expected ExceededWarnings to be non-empty when threshold is exceeded")
	}
	for _, w := range res.ExceededWarnings {
		if !strings.Contains(w, "exceeded max lock duration") {
			t.Errorf("expected warning to mention 'exceeded max lock duration', got %q", w)
		}
	}
}

func TestSimulateSQLite_ConstraintViolation_Rollback(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER NOT NULL);`); err != nil {
		t.Fatalf("seeding table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO accounts (balance) VALUES (100), (200);`); err != nil {
		t.Fatalf("seeding rows: %v", err)
	}

	beforeSnapshot := takeSQLiteSnapshot(t, db)

	// Add a NOT NULL column without a default to a table with existing rows.
	desired := `
		CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER NOT NULL, active INTEGER NOT NULL);
	`

	metrics := &simMockMetrics{}
	_, err = exec.SimulateSQLite(ctx, db, exec.SQLiteExecConfig{
		SchemaSQL:     desired,
		AcceptHazards: []plan.HazardCode{plan.HazardNotNullNoDefault},
		Metrics:       metrics,
	}, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected simulation to fail on constraint violation")
	}

	afterSnapshot := takeSQLiteSnapshot(t, db)
	if beforeSnapshot != afterSnapshot {
		t.Fatalf("schema catalog changed after failed simulation:\nbefore: %s\nafter:  %s", beforeSnapshot, afterSnapshot)
	}

	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if len(metrics.simulations) != 1 {
		t.Fatalf("expected 1 simulation metric call on failure, got %d", len(metrics.simulations))
	}
	if metrics.simulations[0].status != "failed" {
		t.Errorf("expected status 'failed', got %q", metrics.simulations[0].status)
	}
}

func TestSimulateSQLite_NoopPlan(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, title TEXT NOT NULL);`); err != nil {
		t.Fatalf("seeding table: %v", err)
	}

	desired := `CREATE TABLE items (id INTEGER PRIMARY KEY, title TEXT NOT NULL);`

	metrics := &simMockMetrics{}
	res, err := exec.SimulateSQLite(ctx, db, exec.SQLiteExecConfig{
		SchemaSQL: desired,
		Metrics:   metrics,
	}, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("SimulateSQLite failed: %v", err)
	}
	if len(res.StepTimings) != 0 {
		t.Errorf("expected 0 step timings for no-op, got %d", len(res.StepTimings))
	}

	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if len(metrics.simulations) != 1 || metrics.simulations[0].status != "noop" {
		t.Errorf("expected 1 metric with status 'noop', got %+v", metrics.simulations)
	}
}

func TestSimulateSQLitePlan_NilPlan(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = exec.SimulateSQLitePlan(context.Background(), db, nil, exec.SQLiteExecConfig{}, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error on nil plan")
	}
}
