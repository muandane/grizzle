//go:build slow

package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

type batchCountHandler struct {
	count *int
}

func (h *batchCountHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *batchCountHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "copied keyset batch") {
		*h.count++
	}
	return nil
}
func (h *batchCountHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *batchCountHandler) WithGroup(_ string) slog.Handler      { return h }

func TestSQLite_KeysetBatchCopy_1M_Rows_Checksum(t *testing.T) {
	tmpDir := t.TempDir()
	dbFile := filepath.Join(tmpDir, "test_1m.db")
	dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-64000)", dbFile)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	ctx := t.Context()

	schemaV1 := `
		CREATE TABLE logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			val INTEGER NOT NULL,
			payload TEXT NOT NULL,
			temp_tag TEXT
		);
	`
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaV1,
	}); err != nil {
		t.Fatalf("Sync V1 failed: %v", err)
	}

	t.Log("Seeding 1,000,000 rows into SQLite...")
	seedStart := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO logs (val, payload, temp_tag) VALUES (?, ?, ?);")
	if err != nil {
		t.Fatal(err)
	}
	totalRows := 1000000
	for i := 1; i <= totalRows; i++ {
		if _, err := stmt.ExecContext(ctx, i, "payload_data", "tag"); err != nil {
			t.Fatal(err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("Seeding complete in %v", time.Since(seedStart))

	var preCount, preSumVal int64
	err = db.QueryRowContext(ctx, "SELECT COUNT(*), SUM(val) FROM logs;").Scan(&preCount, &preSumVal)
	if err != nil {
		t.Fatal(err)
	}

	// Schema V2: Drop column 'temp_tag' requiring rebuild with chunked copying
	schemaV2 := `
		CREATE TABLE logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			val INTEGER NOT NULL,
			payload TEXT NOT NULL
		);
	`

	var batchCount int
	batchLogger := slog.New(&batchCountHandler{count: &batchCount})

	rebuildStart := time.Now()
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:                grizzle.DialectSQLite,
		SchemaSQL:              schemaV2,
		AllowDropColumn:        new(true),
		AcceptHazards:          []grizzle.HazardCode{grizzle.HazardDropColumn},
		SQLiteRebuildThreshold: 50000,
		SQLiteRebuildBatchSize: 25000,
		Logger:                 batchLogger,
	})
	rebuildElapsed := time.Since(rebuildStart)
	if err != nil {
		t.Fatalf("Sync V2 chunked rebuild failed: %v", err)
	}
	t.Logf("Rebuilt 1,000,000 row table in %v across %d keyset batches", rebuildElapsed, batchCount)
	if batchCount <= 1 {
		t.Fatalf("expected keyset batch count > 1 for 1M rows, got %d", batchCount)
	}

	var postCount, postSumVal int64
	err = db.QueryRowContext(ctx, "SELECT COUNT(*), SUM(val) FROM logs;").Scan(&postCount, &postSumVal)
	if err != nil {
		t.Fatal(err)
	}
	if postCount != preCount {
		t.Fatalf("row count mismatch: got %d, want %d", postCount, preCount)
	}
	if postSumVal != preSumVal {
		t.Fatalf("checksum mismatch: got %d, want %d", postSumVal, preSumVal)
	}
	t.Logf("1M row verification passed: count=%d, checksum=%d, batchCount=%d", postCount, postSumVal, batchCount)
}
