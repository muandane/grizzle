//go:build scenario

package scenarios

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle/internal/exec"
)

// BenchmarkKeysetBackfill_100kRows benchmarks keyset pagination across 100,000 rows
// with a 3-column composite primary key (tenant_id, category, id).
func BenchmarkKeysetBackfill_100kRows(b *testing.B) {
	ctx := context.Background()
	dsn := getTestDSN()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		b.Fatalf("opening database: %v", err)
	}
	defer func() { _ = db.Close() }()

	const testSchema = "scenario_bench"
	_, err = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE; CREATE SCHEMA %q;", testSchema, testSchema))
	if err != nil {
		b.Fatalf("reset schema: %v", err)
	}

	// Create table with 3-column composite PK
	createSQL := fmt.Sprintf(`
CREATE TABLE %q.tenant_events (
    tenant_id INT NOT NULL,
    category TEXT NOT NULL,
    id BIGINT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    archived_at TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, category, id)
);
`, testSchema)
	if _, err := db.Exec(createSQL); err != nil {
		b.Fatalf("create table: %v", err)
	}

	// Seed 100,000 rows using generate_series
	b.Log("Seeding 100,000 rows with 3-column composite PK...")
	seedSQL := fmt.Sprintf(`
INSERT INTO %q.tenant_events (tenant_id, category, id, created_at)
SELECT (g %% 100) + 1,
       CASE (g %% 4)
           WHEN 0 THEN 'audit'
           WHEN 1 THEN 'billing'
           WHEN 2 THEN 'security'
           ELSE 'system'
       END,
       g,
       NOW() - (g || ' minutes')::interval
FROM generate_series(1, 100000) AS g;
`, testSchema)

	seedStart := time.Now()
	if _, err := db.Exec(seedSQL); err != nil {
		b.Fatalf("seeding rows: %v", err)
	}
	b.Logf("✓ 100,000 rows seeded in %s", time.Since(seedStart))

	batchSizes := []int{500, 1000, 5000}
	for _, batch := range batchSizes {
		b.Run(fmt.Sprintf("BatchSize_%d", batch), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// Reset archived_at to NULL
				_, err := db.Exec(fmt.Sprintf("UPDATE %q.tenant_events SET archived_at = NULL;", testSchema))
				if err != nil {
					b.Fatalf("resetting archived_at: %v", err)
				}

				hook := exec.NewCopyBackfillHook(exec.DialectPostgres, batch)
				b.StartTimer()

				start := time.Now()
				err = exec.RunBackfill(ctx, db, testSchema, map[string]string{
					fmt.Sprintf("%s.tenant_events.created_at", testSchema): "archived_at",
				}, nil, hook, nil)
				elapsed := time.Since(start)

				if err != nil {
					b.Fatalf("run backfill: %v", err)
				}

				throughput := float64(100000) / elapsed.Seconds()
				b.ReportMetric(throughput, "rows/sec")
			}
		})
	}
}
