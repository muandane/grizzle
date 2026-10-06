// Package griztest provides testing utilities and ergonomic primitives
// for testing applications powered by Grizzle schema migrations.
package griztest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/muandane/grizzle"
)

// MustSync applies the given schemaSQL to db using Grizzle.
// It defaults to AllowDrop: true so that tests can cleanly reset, rebuild, or mutate
// existing test schemas without manual teardown scripts.
// Additional functional options can be passed to override or configure settings
// (e.g., grizzle.WithDialect, grizzle.WithTargetSchemas).
//
// If synchronization fails, MustSync aborts the test immediately via t.Fatalf.
func MustSync(t testing.TB, db *sql.DB, schemaSQL string, opts ...grizzle.Option) {
	t.Helper()

	options := grizzle.Options{
		SchemaSQL: schemaSQL,
		AllowDrop: true,
	}
	for _, opt := range opts {
		opt(&options)
	}

	if err := grizzle.Sync(context.Background(), db, options); err != nil {
		t.Fatalf("griztest: sync failed: %v", err)
	}
}

// MustPlan computes the execution plan for the given schemaSQL without applying it.
// It defaults to AllowDrop: true and applies any additional functional options.
//
// If plan computation fails, MustPlan aborts the test immediately via t.Fatalf.
func MustPlan(t testing.TB, db *sql.DB, schemaSQL string, opts ...grizzle.Option) *grizzle.Plan {
	t.Helper()

	options := grizzle.Options{
		SchemaSQL: schemaSQL,
		AllowDrop: true,
	}
	for _, opt := range opts {
		opt(&options)
	}

	p, err := grizzle.PlanDiff(context.Background(), db, options)
	if err != nil {
		t.Fatalf("griztest: plan diff failed: %v", err)
	}
	return p
}

// Reset is an alias for MustSync, providing clear semantic intent when resetting
// or rebuilding database state during test setup or teardown.
func Reset(t testing.TB, db *sql.DB, schemaSQL string, opts ...grizzle.Option) {
	t.Helper()
	MustSync(t, db, schemaSQL, opts...)
}
