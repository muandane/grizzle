//go:build integration

package exec_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestCheck_ValidateConstraintRunsInSeparateTxAfterCommit mirrors the foreign
// key staging test for CHECK constraints: ADD ... NOT VALID must commit in its
// own transaction before VALIDATE CONSTRAINT runs in a separate one, so a
// validation failure leaves the NOT VALID constraint in the catalog instead of
// rolling back the whole migration.
func TestCheck_ValidateConstraintRunsInSeparateTxAfterCommit(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_chk_sep_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	// 1. Create products table with a row that violates the desired CHECK.
	initSQL := fmt.Sprintf(`
		CREATE TABLE %s.products (id INT PRIMARY KEY, price_cents INT NOT NULL);
		INSERT INTO %s.products (id, price_cents) VALUES (1, -500);
	`, schema, schema)
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("failed creating initial table: %v", err)
	}

	// 2. Desired schema adds a CHECK constraint that the existing row violates.
	desiredSQL := `
		CREATE TABLE products (
			id INT PRIMARY KEY,
			price_cents INT NOT NULL,
			CONSTRAINT check_positive_price CHECK (price_cents > 0)
		);
	`

	cfg := exec.PostgresExecConfig{
		TargetSchema:     schema,
		ShadowSchema:     fmt.Sprintf("_shadow_%s", schema),
		SchemaSQL:        desiredSQL,
		LockID:           time.Now().UnixNano(),
		Policy:           plan.DropPolicy{},
		LockTimeout:      5 * time.Second,
		StatementTimeout: 10 * time.Second,
	}

	// 3. SyncPostgres should fail on VALIDATE CONSTRAINT because row 1 violates the check.
	err = exec.SyncPostgres(context.Background(), db, cfg)
	if err == nil {
		t.Fatalf("expected SyncPostgres to fail on VALIDATE CONSTRAINT with violating rows, got nil")
	}

	// 4. CRUCIAL ASSERTION: because ADD CONSTRAINT ... NOT VALID committed in a
	// separate transaction before VALIDATE, the constraint must exist in
	// pg_constraint with convalidated = false.
	var convalidated bool
	query := fmt.Sprintf(`
		SELECT convalidated
		FROM pg_constraint
		WHERE conname = 'check_positive_price'
		  AND conrelid = '%s.products'::regclass;
	`, schema)
	if err := db.QueryRow(query).Scan(&convalidated); err != nil {
		t.Fatalf("expected check_positive_price to exist in catalog (proving ADD NOT VALID committed in separate tx), err: %v", err)
	}
	if convalidated {
		t.Errorf("expected convalidated to be false before VALIDATE CONSTRAINT succeeds")
	}

	// 5. Repair the violating data.
	if _, err := db.Exec(fmt.Sprintf("UPDATE %s.products SET price_cents = 500 WHERE id = 1;", schema)); err != nil {
		t.Fatalf("failed repairing data: %v", err)
	}

	// 6. Run sync again: only VALIDATE CONSTRAINT remains, and it succeeds.
	if err := exec.SyncPostgres(context.Background(), db, cfg); err != nil {
		t.Fatalf("expected SyncPostgres to succeed after repairing data, got: %v", err)
	}

	// 7. The constraint is now validated.
	if err := db.QueryRow(query).Scan(&convalidated); err != nil {
		t.Fatalf("failed re-reading constraint state: %v", err)
	}
	if !convalidated {
		t.Errorf("expected convalidated to be true after successful VALIDATE CONSTRAINT")
	}

	// 8. Third sync is a no-op.
	if err := exec.SyncPostgres(context.Background(), db, cfg); err != nil {
		t.Fatalf("expected idempotent no-op sync, got: %v", err)
	}
}

// TestCheck_AddRedefineDrop exercises the full CHECK lifecycle: add, redefine
// (drop + add), and drop of an explicitly named constraint.
func TestCheck_AddRedefineDrop(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_chk_lifecycle_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	syncWithSchema := func(desiredSQL string) error {
		return exec.SyncPostgres(context.Background(), db, exec.PostgresExecConfig{
			TargetSchema: schema,
			ShadowSchema: fmt.Sprintf("_shadow_%s", schema),
			SchemaSQL:    desiredSQL,
			LockID:       time.Now().UnixNano(),
			Policy: plan.DropPolicy{
				AllowTable: true,
				AllowCheck: true,
			},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 10 * time.Second,
		})
	}

	// 1. Add a named CHECK.
	base := `
		CREATE TABLE products (
			id INT PRIMARY KEY,
			price_cents INT NOT NULL,
			CONSTRAINT check_positive_price CHECK (price_cents > 0)
		);
	`
	if err := syncWithSchema(base); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// 2. Redefine the CHECK (drop + add).
	redefined := `
		CREATE TABLE products (
			id INT PRIMARY KEY,
			price_cents INT NOT NULL,
			CONSTRAINT check_positive_price CHECK (price_cents >= 100)
		);
	`
	if err := syncWithSchema(redefined); err != nil {
		t.Fatalf("redefine sync failed: %v", err)
	}

	// 3. Drop the CHECK (orphan).
	orphaned := `
		CREATE TABLE products (
			id INT PRIMARY KEY,
			price_cents INT NOT NULL
		);
	`
	if err := syncWithSchema(orphaned); err != nil {
		t.Fatalf("drop sync failed: %v", err)
	}

	var count int
	if err := db.QueryRow(fmt.Sprintf(
		"SELECT count(*) FROM pg_constraint WHERE conrelid = '%s.products'::regclass AND contype = 'c';",
		schema)).Scan(&count); err != nil {
		t.Fatalf("failed counting check constraints: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 check constraints after drop sync, got %d", count)
	}
}
