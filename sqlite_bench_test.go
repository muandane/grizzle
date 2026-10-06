//go:build bench

package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

// BenchmarkSQLite_RebuildChunkSize measures SQLite rebuild execution timing and WAL checkpoints
// across different chunk sizes (1,000, 5,000, 10,000, 50,000).
// Run with: go test -tags bench -bench BenchmarkSQLite_RebuildChunkSize -benchtime 1x .
func BenchmarkSQLite_RebuildChunkSize(b *testing.B) {
	chunkSizes := []int{1000, 5000, 10000, 50000}
	totalRows := 100000

	for _, chunkSize := range chunkSizes {
		b.Run(fmt.Sprintf("chunk_%d", chunkSize), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				tmpDir := b.TempDir()
				dbFile := filepath.Join(tmpDir, "bench_rebuild.db")
				dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", dbFile)

				db, err := sql.Open("sqlite", dsn)
				if err != nil {
					b.Fatalf("open sqlite: %v", err)
				}
				db.SetMaxOpenConns(1)

				ctx := context.Background()

				// Schema V1
				schemaV1 := `
					CREATE TABLE records (
						id INTEGER PRIMARY KEY AUTOINCREMENT,
						val INTEGER NOT NULL,
						meta TEXT,
						temp_col TEXT
					);
				`
				if err := grizzle.Sync(ctx, db, grizzle.Options{
					Dialect:   grizzle.DialectSQLite,
					SchemaSQL: schemaV1,
				}); err != nil {
					b.Fatalf("Sync V1: %v", err)
				}

				// Seed totalRows
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					b.Fatal(err)
				}
				stmt, err := tx.PrepareContext(ctx, "INSERT INTO records (val, meta, temp_col) VALUES (?, ?, ?);")
				if err != nil {
					b.Fatal(err)
				}
				for i := 1; i <= totalRows; i++ {
					if _, err := stmt.ExecContext(ctx, i, "metadata", "temp"); err != nil {
						b.Fatal(err)
					}
				}
				_ = stmt.Close()
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}

				// Checkpoint before rebuild
				var busy, log, chkpt int
				_ = db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE);").Scan(&busy, &log, &chkpt)

				// Schema V2: Drop temp_col, triggering rebuild
				schemaV2 := `
					CREATE TABLE records (
						id INTEGER PRIMARY KEY AUTOINCREMENT,
						val INTEGER NOT NULL,
						meta TEXT
					);
				`

				start := time.Now()
				err = grizzle.Sync(ctx, db, grizzle.Options{
					Dialect:                grizzle.DialectSQLite,
					SchemaSQL:              schemaV2,
					AllowDropColumn:        new(true),
					AcceptHazards:          []grizzle.HazardCode{grizzle.HazardDropColumn},
					SQLiteRebuildThreshold: 1000,
					SQLiteRebuildBatchSize: chunkSize,
				})
				elapsed := time.Since(start)

				if err != nil {
					b.Fatalf("Sync rebuild failed: %v", err)
				}

				_ = db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE);").Scan(&busy, &log, &chkpt)
				b.ReportMetric(float64(elapsed.Milliseconds()), "ms/rebuild")
				b.ReportMetric(float64(log), "wal_pages")

				_ = db.Close()
			}
		})
	}
}
