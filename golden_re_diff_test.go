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

func TestIntegration_ApplyThenReDiffEmpty(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schemas := []struct {
		name string
		sql  string
	}{
		{
			name: "basic_tables_and_indexes",
			sql: `
				CREATE TABLE items (
					id BIGINT PRIMARY KEY,
					sku TEXT NOT NULL,
					created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
				);
				CREATE UNIQUE INDEX idx_items_sku ON items (sku);
			`,
		},
		{
			name: "check_constraints_and_enums",
			sql: `
				CREATE TYPE priority_level AS ENUM ('low', 'medium', 'high');
				CREATE TABLE tasks (
					id BIGINT PRIMARY KEY,
					priority priority_level NOT NULL,
					rating INT,
					CONSTRAINT chk_rating CHECK (rating >= 1 AND rating <= 5)
				);
			`,
		},
	}

	for _, tc := range schemas {
		t.Run(tc.name, func(t *testing.T) {
			schemaName := fmt.Sprintf("test_rediff_%d", time.Now().UnixNano())
			_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %q;", schemaName))
			if err != nil {
				t.Fatalf("failed creating schema: %v", err)
			}
			defer func() {
				_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %q CASCADE;", schemaName))
			}()

			opts := grizzle.Options{
				Dialect:      grizzle.DialectPostgres,
				TargetSchema: schemaName,
				SchemaSQL:    tc.sql,
			}

			if err := grizzle.Sync(ctx, db, opts); err != nil {
				t.Fatalf("initial Sync failed for %s: %v", tc.name, err)
			}

			p, err := grizzle.PlanDiff(ctx, db, opts)
			if err != nil {
				t.Fatalf("re-diff PlanDiff failed for %s: %v", tc.name, err)
			}

			if len(p.Steps) != 0 {
				t.Errorf("expected 0 steps on re-diff for %s, got %d steps", tc.name, len(p.Steps))
			}
		})
	}
}
