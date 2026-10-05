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

	"github.com/yourorg/grizzle"
)

func TestExpandContract_AmbiguousRenameEmitsHazard(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer db.Close()

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
	defer db.Close()

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
	defer db.Close()

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
