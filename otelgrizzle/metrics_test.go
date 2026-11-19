package otelgrizzle_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/otelgrizzle"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	_ "modernc.org/sqlite"
)

func TestOTELGrizzle_Metrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter("grizzle-test")

	m, err := otelgrizzle.NewMetrics(meter)
	if err != nil {
		t.Fatalf("NewMetrics failed: %v", err)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL
);`

	opts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schema,
	}
	otelgrizzle.WithMetrics(m)(&opts)

	// 1. Initial sync (status: "applied")
	err = grizzle.Sync(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("grizzle sync failed: %v", err)
	}

	// 2. Second sync with no changes (status: "noop")
	err = grizzle.Sync(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("noop sync failed: %v", err)
	}

	// 3. Check for drift (should be 0 drift detected initially)
	err = grizzle.Check(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}

	// 4. Introduce drift in desired schema and run Check
	driftOpts := opts
	driftOpts.SchemaSQL = `
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT
);`
	err = grizzle.Check(context.Background(), db, driftOpts)
	if err == nil {
		t.Fatalf("expected DriftError, got nil")
	}

	// 5. Test manual metric calls (e.g. lock wait, simulation duration)
	m.RecordLockWait(context.Background(), 25*time.Millisecond, "sqlite")
	if sm, ok := m.(interface {
		RecordSimulationDuration(ctx context.Context, duration time.Duration, dialect string, status string)
	}); ok {
		sm.RecordSimulationDuration(context.Background(), 50*time.Millisecond, "sqlite", "applied")
	}

	// Collect metrics from the reader
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}

	foundDuration := false
	foundDrift := false
	foundLockWait := false
	foundSimulation := false

	for _, sm := range rm.ScopeMetrics {
		for _, metric := range sm.Metrics {
			switch metric.Name {
			case "migration_duration_seconds":
				foundDuration = true
				h, ok := metric.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("expected Histogram[float64], got %T", metric.Data)
				}
				if len(h.DataPoints) < 2 {
					t.Errorf("expected at least 2 duration data points (applied, noop), got %d", len(h.DataPoints))
				}
			case "drift_detected":
				foundDrift = true
				c, ok := metric.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("expected Sum[int64], got %T", metric.Data)
				}
				if len(c.DataPoints) == 0 || c.DataPoints[0].Value < 1 {
					t.Errorf("expected drift_detected >= 1, got %+v", c.DataPoints)
				}
			case "lock_wait_seconds":
				foundLockWait = true
				h, ok := metric.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("expected Histogram[float64], got %T", metric.Data)
				}
				if len(h.DataPoints) == 0 {
					t.Errorf("expected lock_wait data points, got 0")
				}
			case "simulation_duration_seconds":
				foundSimulation = true
				h, ok := metric.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("expected Histogram[float64], got %T", metric.Data)
				}
				if len(h.DataPoints) == 0 {
					t.Errorf("expected simulation_duration data points, got 0")
				}
			}
		}
	}

	if !foundDuration {
		t.Errorf("metric migration_duration_seconds not found")
	}
	if !foundDrift {
		t.Errorf("metric drift_detected not found")
	}
	if !foundLockWait {
		t.Errorf("metric lock_wait_seconds not found")
	}
	if !foundSimulation {
		t.Errorf("metric simulation_duration_seconds not found")
	}
}

func TestOTELGrizzle_NewMetrics_NilMeter(t *testing.T) {
	_, err := otelgrizzle.NewMetrics(nil)
	if err == nil {
		t.Fatalf("expected error when meter is nil, got nil")
	}
}
