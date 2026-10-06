package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

func TestOptions_ZeroValueSafety(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var opts grizzle.Options // Zero-value Options

	// 1. Validate on zero-value Options returns ErrEmptySchema without panic
	if err := opts.Validate(); !errors.Is(err, grizzle.ErrEmptySchema) {
		t.Fatalf("expected ErrEmptySchema on zero-value options Validate(), got: %v", err)
	}

	// 2. Open an in-memory SQLite database
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	// 3. Sync on zero-value Options returns ErrEmptySchema without panic
	if err := grizzle.Sync(ctx, db, opts); !errors.Is(err, grizzle.ErrEmptySchema) {
		t.Fatalf("expected ErrEmptySchema on Sync with zero-value options, got: %v", err)
	}

	// 4. PlanDiff on zero-value Options returns ErrEmptySchema without panic
	p, err := grizzle.PlanDiff(ctx, db, opts)
	if !errors.Is(err, grizzle.ErrEmptySchema) {
		t.Fatalf("expected ErrEmptySchema on PlanDiff with zero-value options, got: %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil Plan on error, got: %+v", p)
	}

	// 5. Check on zero-value Options returns ErrEmptySchema without panic
	if err := grizzle.Check(ctx, db, opts); !errors.Is(err, grizzle.ErrEmptySchema) {
		t.Fatalf("expected ErrEmptySchema on Check with zero-value options, got: %v", err)
	}

	// 6. Minimal valid SchemaSQL with otherwise zero-value Options defaults safely
	minimalOpts := grizzle.Options{
		SchemaSQL: "CREATE TABLE zero_test (id INTEGER PRIMARY KEY);",
	}

	// Default-deny drops: AllowDrop is false
	if minimalOpts.AllowDrop {
		t.Fatalf("expected AllowDrop to be false by default")
	}

	// Sync succeeds using auto-detected dialect, default target schema, safe defaults
	if err := grizzle.Sync(ctx, db, minimalOpts); err != nil {
		t.Fatalf("expected Sync with minimal options to succeed, got: %v", err)
	}

	// Subsequent check reports in sync
	if err := grizzle.Check(ctx, db, minimalOpts); err != nil {
		t.Fatalf("expected Check to report in sync, got: %v", err)
	}
}
