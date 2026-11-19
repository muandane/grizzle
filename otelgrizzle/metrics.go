// Package otelgrizzle provides an OpenTelemetry instrumentation adapter for Grizzle.
package otelgrizzle

import (
	"context"
	"fmt"
	"time"

	"github.com/muandane/grizzle"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type otelMetrics struct {
	migrationDuration  metric.Float64Histogram
	driftDetected      metric.Int64Counter
	lockWait           metric.Float64Histogram
	simulationDuration metric.Float64Histogram
}

// NewMetrics creates a grizzle.Metrics adapter configured with the provided OpenTelemetry metric.Meter.
// It registers the following metrics:
//   - migration_duration_seconds (Float64Histogram): End-to-end migration duration labeled by dialect and status ("applied", "failed", "noop").
//   - drift_detected (Int64Counter): Total count of detected schema or plan drift events labeled by dialect and schema.
//   - lock_wait_seconds (Float64Histogram): Time spent acquiring or waiting for database locks in seconds.
//   - simulation_duration_seconds (Float64Histogram): Duration of migration simulations in seconds labeled by dialect and status.
func NewMetrics(meter metric.Meter) (grizzle.Metrics, error) {
	if meter == nil {
		return nil, fmt.Errorf("otelgrizzle: meter cannot be nil")
	}

	migrationDuration, err := meter.Float64Histogram(
		"migration_duration_seconds",
		metric.WithDescription("Duration of database migrations in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("otelgrizzle: creating migration_duration_seconds histogram: %w", err)
	}

	driftDetected, err := meter.Int64Counter(
		"drift_detected",
		metric.WithDescription("Total count of detected schema or plan drift events"),
		metric.WithUnit("{events}"),
	)
	if err != nil {
		return nil, fmt.Errorf("otelgrizzle: creating drift_detected counter: %w", err)
	}

	lockWait, err := meter.Float64Histogram(
		"lock_wait_seconds",
		metric.WithDescription("Time spent acquiring or waiting for database locks in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("otelgrizzle: creating lock_wait_seconds histogram: %w", err)
	}

	simulationDuration, err := meter.Float64Histogram(
		"simulation_duration_seconds",
		metric.WithDescription("Duration of migration simulations in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("otelgrizzle: creating simulation_duration_seconds histogram: %w", err)
	}

	return &otelMetrics{
		migrationDuration:  migrationDuration,
		driftDetected:      driftDetected,
		lockWait:           lockWait,
		simulationDuration: simulationDuration,
	}, nil
}

// WithMetrics returns a grizzle.Option configuring OpenTelemetry metrics.
func WithMetrics(m grizzle.Metrics) grizzle.Option {
	return grizzle.WithMetrics(m)
}

// RecordMigrationDuration records the end-to-end duration and status of a migration.
func (m *otelMetrics) RecordMigrationDuration(ctx context.Context, duration time.Duration, dialect string, status string) {
	if m == nil || m.migrationDuration == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("dialect", dialect),
		attribute.String("status", status),
	}
	m.migrationDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordDriftDetected increments the drift_detected counter when schema or plan drift occurs.
func (m *otelMetrics) RecordDriftDetected(ctx context.Context, dialect string, schema string) {
	if m == nil || m.driftDetected == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("dialect", dialect),
	}
	if schema != "" {
		attrs = append(attrs, attribute.String("schema", schema))
	}
	m.driftDetected.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RecordLockWait records the duration spent waiting for or acquiring database locks.
func (m *otelMetrics) RecordLockWait(ctx context.Context, duration time.Duration, dialect string) {
	if m == nil || m.lockWait == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("dialect", dialect),
	}
	m.lockWait.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

// RecordSimulationDuration records the end-to-end duration and status of a migration simulation.
func (m *otelMetrics) RecordSimulationDuration(ctx context.Context, duration time.Duration, dialect string, status string) {
	if m == nil || m.simulationDuration == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("dialect", dialect),
		attribute.String("status", status),
	}
	m.simulationDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}
