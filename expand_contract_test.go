package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muandane/grizzle"
)

func TestExpandContract_AmbiguousRenameEmitsHazard(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Initial schema
	initSQL := `CREATE TABLE users (id INTEGER PRIMARY KEY, first_name TEXT);`
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: initSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Desired schema replaces first_name with given_name (both TEXT, same table)
	desiredSQL := `CREATE TABLE users (id INTEGER PRIMARY KEY, given_name TEXT);`

	trueVal := true
	// 1. Without Options.Renames: should emit HazardRenameAmbiguous
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:         grizzle.DialectSQLite,
		SchemaSQL:       desiredSQL,
		AllowDropColumn: &trueVal,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	var hasAmbiguousHazard bool
	for _, h := range p.Hazards() {
		if h.Code == grizzle.HazardRenameAmbiguous {
			hasAmbiguousHazard = true
			if h.Level != grizzle.HazardLevelCritical {
				t.Errorf("expected RENAME_AMBIGUOUS to be CRITICAL, got %s", h.Level)
			}
		}
	}
	if !hasAmbiguousHazard {
		t.Fatalf("expected HazardRenameAmbiguous to be emitted for unmapped candidate rename, got hazards: %+v", p.Hazards())
	}

	// 2. Applying without accepting hazard should fail with ErrHazardBlocked
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{})
	if !errors.Is(err, grizzle.ErrHazardBlocked) {
		t.Fatalf("expected Apply to fail with ErrHazardBlocked, got: %v", err)
	}

	// 3. Applying accepting only DROP_COLUMN still fails because RENAME_AMBIGUOUS is unaccepted
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{
		AcceptHazards: []grizzle.HazardCode{grizzle.HazardDropColumn},
	})
	if !errors.Is(err, grizzle.ErrHazardBlocked) {
		t.Fatalf("expected Apply with only DROP_COLUMN accepted to fail with ErrHazardBlocked, got: %v", err)
	}

	// 4. Applying accepting both DROP_COLUMN and RENAME_AMBIGUOUS succeeds
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{
		AcceptHazards: []grizzle.HazardCode{grizzle.HazardDropColumn, grizzle.HazardRenameAmbiguous},
	})
	if err != nil {
		t.Fatalf("expected Apply to succeed when both hazards accepted, got: %v", err)
	}
}

func TestExpandContract_SingleStepRenameWithMapping(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	initSQL := `CREATE TABLE users (id INTEGER PRIMARY KEY, first_name TEXT);`
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: initSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Insert row to verify data retention
	_, err = db.Exec("INSERT INTO users (id, first_name) VALUES (1, 'Alice');")
	if err != nil {
		t.Fatalf("failed inserting test row: %v", err)
	}

	desiredSQL := `CREATE TABLE users (id INTEGER PRIMARY KEY, given_name TEXT);`

	// Plan with explicit rename mapping
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: desiredSQL,
		Renames: map[string]string{
			"users.first_name": "given_name",
		},
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Should emit single ChangeRenameColumn step
	if len(p.Steps) != 1 {
		t.Fatalf("expected 1 rename step, got %d steps: %+v", len(p.Steps), p.Steps)
	}
	if p.Steps[0].Type != grizzle.ChangeRenameColumn {
		t.Fatalf("expected ChangeRenameColumn, got %s", p.Steps[0].Type)
	}

	// No ambiguous hazard emitted
	for _, h := range p.Hazards() {
		if h.Code == grizzle.HazardRenameAmbiguous {
			t.Fatalf("unexpected HazardRenameAmbiguous after explicit mapping: %+v", h)
		}
	}

	// Apply migration
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Verify data preserved under new column name
	var val string
	err = db.QueryRow("SELECT given_name FROM users WHERE id = 1;").Scan(&val)
	if err != nil || val != "Alice" {
		t.Fatalf("expected data preserved as 'Alice', got %q, err: %v", val, err)
	}
}

func TestExpandContract_PostgresStagedExpand(t *testing.T) {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres expand-contract test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_expand_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initSQL := `CREATE TABLE members (id BIGINT PRIMARY KEY, email_address TEXT);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	desiredSQL := `CREATE TABLE members (id BIGINT PRIMARY KEY, email TEXT);`

	// 1. With ExpandContract: true, emits ADD COLUMN email, but does NOT drop email_address
	planExpand, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:        grizzle.DialectPostgres,
		TargetSchema:   schema,
		SchemaSQL:      desiredSQL,
		ExpandContract: true,
		Renames: map[string]string{
			"members.email_address": "email",
		},
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Assert only ADD COLUMN is emitted, NO DROP COLUMN
	for _, s := range planExpand.Steps {
		if s.Type == grizzle.ChangeDropColumn {
			t.Fatalf("staged expand phase must NOT drop old column, got step: %+v", s)
		}
		if s.Type == grizzle.ChangeAddColumn && !strings.Contains(s.SQL, "email") {
			t.Fatalf("expected step adding email, got %+v", s)
		}
	}

	// Apply expand phase
	err = grizzle.Apply(context.Background(), db, planExpand, grizzle.ApplyOpts{})
	if err != nil {
		t.Fatalf("Apply expand failed: %v", err)
	}

	// Verify BOTH columns exist in postgres
	var colCount int
	err = db.QueryRow(fmt.Sprintf(`
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = '%s' AND table_name = 'members' AND column_name IN ('email_address', 'email');
	`, schema)).Scan(&colCount)
	if err != nil || colCount != 2 {
		t.Fatalf("expected both email_address and email to exist during expand phase, count=%d, err=%v", colCount, err)
	}
}

func TestExpandContract_StagedPlansAndBackfill(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Initial table with rows
	initSQL := `CREATE TABLE members (id INTEGER PRIMARY KEY, email_address TEXT);`
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: initSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	_, err = db.Exec(`INSERT INTO members (id, email_address) VALUES (1, 'alice@example.com'), (2, 'bob@example.com');`)
	if err != nil {
		t.Fatalf("failed inserting test data: %v", err)
	}

	// Desired schema has "email" column instead of "email_address"
	desiredSQL := `CREATE TABLE members (id INTEGER PRIMARY KEY, email TEXT NOT NULL);`

	// ==========================================
	// PLAN 1: Staged Expand
	// ==========================================
	plan1, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:        grizzle.DialectSQLite,
		SchemaSQL:      desiredSQL,
		ExpandContract: true,
		Renames: map[string]string{
			"members.email_address": "email",
		},
	})
	if err != nil {
		t.Fatalf("PlanDiff for plan 1 failed: %v", err)
	}

	// Confirm:
	// 1. Plan 1 adds email column (nullable).
	// 2. Contract step (DROP COLUMN email_address) is NOT in plan 1.
	var hasAddEmail, hasDropEmailAddress bool
	for _, s := range plan1.Steps {
		if strings.Contains(s.SQL, "email_address") && (s.Type == grizzle.ChangeDropColumn || strings.Contains(s.SQL, "DROP")) {
			hasDropEmailAddress = true
		}
		if s.Type == grizzle.ChangeAddColumn && strings.Contains(s.SQL, "email") {
			hasAddEmail = true
			if strings.Contains(s.SQL, "NOT NULL") {
				t.Fatalf("staged expand add column must be nullable, got SQL: %s", s.SQL)
			}
		}
	}

	if !hasAddEmail {
		t.Fatalf("expected plan 1 to add email column, got steps: %+v", plan1.Steps)
	}
	if hasDropEmailAddress {
		t.Fatalf("contract step (drop old column) must NOT be in plan 1, got steps: %+v", plan1.Steps)
	}

	plan1Hash := plan1.Hash()

	// Apply Plan 1 with Options.Backfill hook
	var backfillBatches int
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:        grizzle.DialectSQLite,
		SchemaSQL:      desiredSQL,
		ExpandContract: true,
		Renames: map[string]string{
			"members.email_address": "email",
		},
		Backfill: func(ctx context.Context, tx *sql.Tx, table, oldCol, newCol string) error {
			backfillBatches++
			_, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s IS NULL", table, newCol, oldCol, newCol))
			return err
		},
	})
	if err != nil {
		t.Fatalf("apply plan 1 with backfill failed: %v", err)
	}

	if backfillBatches == 0 {
		t.Fatalf("expected Options.Backfill hook to be called, got 0 calls")
	}

	// Verify both columns exist and data was backfilled
	rows, err := db.Query("SELECT id, email_address, email FROM members ORDER BY id ASC")
	if err != nil {
		t.Fatalf("failed querying members after expand: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type memberRow struct {
		id   int
		oldE string
		newE string
	}
	var data []memberRow
	for rows.Next() {
		var r memberRow
		if err := rows.Scan(&r.id, &r.oldE, &r.newE); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		data = append(data, r)
	}
	if len(data) != 2 || data[0].newE != "alice@example.com" || data[1].newE != "bob@example.com" {
		t.Fatalf("unexpected data after backfill: %+v", data)
	}

	// ==========================================
	// PLAN 2: Contract (separate, separately hashed/approved)
	// ==========================================
	trueVal := true
	plan2, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:         grizzle.DialectSQLite,
		SchemaSQL:       desiredSQL,
		AllowDropColumn: &trueVal,
		Renames: map[string]string{
			"members.email_address": "email",
		},
	})
	if err != nil {
		t.Fatalf("PlanDiff for plan 2 failed: %v", err)
	}

	plan2Hash := plan2.Hash()
	if plan2Hash == plan1Hash {
		t.Fatalf("plan 2 hash (%s) must differ from plan 1 hash (%s)", plan2Hash, plan1Hash)
	}

	var plan2HasDrop bool
	for _, s := range plan2.Steps {
		if s.Type == grizzle.ChangeDropColumn || strings.Contains(s.SQL, "email_address") || s.IsTableRebuild {
			plan2HasDrop = true
		}
	}
	if !plan2HasDrop {
		t.Fatalf("expected plan 2 to drop old column, got steps: %+v", plan2.Steps)
	}

	// Apply Plan 2 (contract)
	err = grizzle.Apply(ctx, db, plan2, grizzle.ApplyOpts{
		ExpectedHash: plan2Hash,
		AcceptHazards: []grizzle.HazardCode{
			grizzle.HazardDropColumn,
		},
	})
	if err != nil {
		t.Fatalf("apply plan 2 failed: %v", err)
	}

	// Verify old column is gone and new column remains
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM members WHERE email IS NOT NULL").Scan(&count)
	if err != nil {
		t.Fatalf("failed querying members after contract: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 members with email, got %d", count)
	}
}

func TestExpandContract_PostgresStagedPlansAndBackfill(t *testing.T) {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres expand-contract staged test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_staged_expand_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	ctx := context.Background()

	initSQL := `CREATE TABLE members (id BIGINT PRIMARY KEY, email_address TEXT);`
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	_, err = db.Exec(fmt.Sprintf(`INSERT INTO %s.members (id, email_address) VALUES (1, 'alice@example.com'), (2, 'bob@example.com');`, schema))
	if err != nil {
		t.Fatalf("failed inserting test data: %v", err)
	}

	desiredSQL := `CREATE TABLE members (id BIGINT PRIMARY KEY, email TEXT NOT NULL);`

	// PLAN 1: Staged Expand
	plan1, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:        grizzle.DialectPostgres,
		TargetSchema:   schema,
		SchemaSQL:      desiredSQL,
		ExpandContract: true,
		Renames: map[string]string{
			"members.email_address": "email",
		},
	})
	if err != nil {
		t.Fatalf("PlanDiff for plan 1 failed: %v", err)
	}

	var hasAddEmail, hasDropEmailAddress bool
	for _, s := range plan1.Steps {
		if strings.Contains(s.SQL, "email_address") && (s.Type == grizzle.ChangeDropColumn || strings.Contains(s.SQL, "DROP")) {
			hasDropEmailAddress = true
		}
		if s.Type == grizzle.ChangeAddColumn && strings.Contains(s.SQL, "email") {
			hasAddEmail = true
			if strings.Contains(s.SQL, "NOT NULL") {
				t.Fatalf("staged expand add column must be nullable, got SQL: %s", s.SQL)
			}
		}
	}

	if !hasAddEmail {
		t.Fatalf("expected plan 1 to add email column, got steps: %+v", plan1.Steps)
	}
	if hasDropEmailAddress {
		t.Fatalf("contract step (drop old column) must NOT be in plan 1, got steps: %+v", plan1.Steps)
	}

	plan1Hash := plan1.Hash()

	var backfillBatches int
	err = grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:        grizzle.DialectPostgres,
		TargetSchema:   schema,
		SchemaSQL:      desiredSQL,
		ExpandContract: true,
		Renames: map[string]string{
			"members.email_address": "email",
		},
		Backfill: func(ctx context.Context, tx *sql.Tx, table, oldCol, newCol string) error {
			backfillBatches++
			_, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s.%s SET %s = %s WHERE %s IS NULL", schema, table, newCol, oldCol, newCol))
			return err
		},
	})
	if err != nil {
		t.Fatalf("apply plan 1 with backfill failed: %v", err)
	}

	if backfillBatches == 0 {
		t.Fatalf("expected Backfill hook to be called, got 0 calls")
	}

	// Verify both columns exist and data was backfilled
	rows, err := db.Query(fmt.Sprintf("SELECT id, email_address, email FROM %s.members ORDER BY id ASC", schema))
	if err != nil {
		t.Fatalf("failed querying members after expand: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type memberRow struct {
		id   int64
		oldE string
		newE string
	}
	var data []memberRow
	for rows.Next() {
		var r memberRow
		if err := rows.Scan(&r.id, &r.oldE, &r.newE); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		data = append(data, r)
	}
	if len(data) != 2 || data[0].newE != "alice@example.com" || data[1].newE != "bob@example.com" {
		t.Fatalf("unexpected data after backfill: %+v", data)
	}

	// PLAN 2: Contract
	trueVal := true
	plan2, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:         grizzle.DialectPostgres,
		TargetSchema:    schema,
		SchemaSQL:       desiredSQL,
		AllowDropColumn: &trueVal,
		Renames: map[string]string{
			"members.email_address": "email",
		},
	})
	if err != nil {
		t.Fatalf("PlanDiff for plan 2 failed: %v", err)
	}

	plan2Hash := plan2.Hash()
	if plan2Hash == plan1Hash {
		t.Fatalf("plan 2 hash (%s) must differ from plan 1 hash (%s)", plan2Hash, plan1Hash)
	}

	var plan2HasDrop bool
	for _, s := range plan2.Steps {
		if s.Type == grizzle.ChangeDropColumn && strings.Contains(s.SQL, "email_address") {
			plan2HasDrop = true
		}
	}
	if !plan2HasDrop {
		t.Fatalf("expected plan 2 to drop old column, got steps: %+v", plan2.Steps)
	}

	err = grizzle.Apply(ctx, db, plan2, grizzle.ApplyOpts{
		ExpectedHash: plan2Hash,
		AcceptHazards: []grizzle.HazardCode{
			grizzle.HazardDropColumn,
		},
	})
	if err != nil {
		t.Fatalf("apply plan 2 failed: %v", err)
	}

	var count int
	err = db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s.members WHERE email IS NOT NULL", schema)).Scan(&count)
	if err != nil {
		t.Fatalf("failed querying members after contract: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 members with email, got %d", count)
	}
}


