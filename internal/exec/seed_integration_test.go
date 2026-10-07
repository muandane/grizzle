//go:build integration

package exec_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestSeedPostgres_IdempotencyAndRollback(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := fmt.Sprintf("test_seed_%d", time.Now().UnixNano())
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}

	cfg := exec.SeedExecConfig{TargetSchemas: []string{schema}}
	seedOne := fmt.Sprintf(`
		CREATE TABLE %s.cities (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name TEXT NOT NULL);
		INSERT INTO %s.cities (name) VALUES ('berlin'), ('tokyo');
	`, schema, schema)
	seedTwo := fmt.Sprintf(`INSERT INTO %s.cities (name) VALUES ('lima');`, schema)

	countRows := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s.cities`, schema)).Scan(&n); err != nil {
			t.Fatalf("counting cities: %v", err)
		}
		return n
	}
	appliedSeed := func(hash string) bool {
		t.Helper()
		var n int
		if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s.grizzle_history WHERE seed_hash = $1 AND status = 'applied'`, schema), hash).Scan(&n); err != nil {
			t.Fatalf("querying seed history: %v", err)
		}
		return n > 0
	}

	// 1. First run: seed commits and is recorded.
	if err := exec.SeedPostgres(ctx, db, cfg, seedOne); err != nil {
		t.Fatalf("SeedPostgres failed: %v", err)
	}
	if got := countRows(); got != 2 {
		t.Errorf("rows after first seed = %d, want 2", got)
	}
	hashOne := exec.SeedHash(seedOne)
	if !appliedSeed(hashOne) {
		t.Error("first seed not recorded as applied in grizzle_history")
	}

	// 2. Same hash: skipped (no duplicate rows, no re-execution).
	if err := exec.SeedPostgres(ctx, db, cfg, seedOne); err != nil {
		t.Fatalf("second SeedPostgres failed: %v", err)
	}
	if got := countRows(); got != 2 {
		t.Errorf("rows after skipped seed = %d, want 2", got)
	}

	// 3. New hash: runs.
	if err := exec.SeedPostgres(ctx, db, cfg, seedTwo); err != nil {
		t.Fatalf("SeedPostgres with new seed failed: %v", err)
	}
	if got := countRows(); got != 3 {
		t.Errorf("rows after new seed = %d, want 3", got)
	}

	// 4. Force re-runs the same hash (script must tolerate re-execution).
	if err := exec.SeedPostgres(ctx, db, exec.SeedExecConfig{TargetSchemas: []string{schema}, Force: true}, seedTwo); err != nil {
		t.Fatalf("forced SeedPostgres failed: %v", err)
	}
	if got := countRows(); got != 4 {
		t.Errorf("rows after forced seed = %d, want 4", got)
	}

	// 5. Bad seed: whole transaction rolls back, schema intact.
	badSeed := fmt.Sprintf(`
			INSERT INTO %s.cities (name) VALUES ('partial');
			INSERT INTO %s.missing_table (name) VALUES ('boom');
		`, schema, schema)
	err := exec.SeedPostgres(ctx, db, cfg, badSeed)
	if !errors.Is(err, plan.ErrSeedFailed) {
		t.Fatalf("expected ErrSeedFailed, got %v", err)
	}
	if got := countRows(); got != 4 {
		t.Errorf("rows after failed seed = %d, want 4 (rollback required)", got)
	}
	if appliedSeed(exec.SeedHash(badSeed)) {
		t.Error("failed seed must not be recorded as applied")
	}
}

func TestSeedSQLite_IdempotencyAndRollback(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	cfg := exec.SeedExecConfig{}
	seedOne := `
		CREATE TABLE cities (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL);
		INSERT INTO cities (name) VALUES ('berlin'), ('tokyo');
	`
	seedTwo := `INSERT INTO cities (name) VALUES ('lima');`

	countRows := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM cities`).Scan(&n); err != nil {
			t.Fatalf("counting cities: %v", err)
		}
		return n
	}

	if err := exec.SeedSQLite(ctx, db, cfg, seedOne); err != nil {
		t.Fatalf("SeedSQLite failed: %v", err)
	}
	if got := countRows(); got != 2 {
		t.Errorf("rows after first seed = %d, want 2", got)
	}

	// Same hash: skipped.
	if err := exec.SeedSQLite(ctx, db, cfg, seedOne); err != nil {
		t.Fatalf("second SeedSQLite failed: %v", err)
	}
	if got := countRows(); got != 2 {
		t.Errorf("rows after skipped seed = %d, want 2", got)
	}

	// New hash: runs.
	if err := exec.SeedSQLite(ctx, db, cfg, seedTwo); err != nil {
		t.Fatalf("SeedSQLite with new seed failed: %v", err)
	}
	if got := countRows(); got != 3 {
		t.Errorf("rows after new seed = %d, want 3", got)
	}

	// Force re-runs.
	if err := exec.SeedSQLite(ctx, db, exec.SeedExecConfig{Force: true}, seedTwo); err != nil {
		t.Fatalf("forced SeedSQLite failed: %v", err)
	}
	if got := countRows(); got != 4 {
		t.Errorf("rows after forced seed = %d, want 4", got)
	}

	// Bad seed rolls back completely.
	badSeed := `
		INSERT INTO cities (name) VALUES ('partial');
		INSERT INTO missing_table (name) VALUES ('boom');
	`
	err = exec.SeedSQLite(ctx, db, cfg, badSeed)
	if !errors.Is(err, plan.ErrSeedFailed) {
		t.Fatalf("expected ErrSeedFailed, got %v", err)
	}
	if got := countRows(); got != 4 {
		t.Errorf("rows after failed seed = %d, want 4 (rollback required)", got)
	}
}
