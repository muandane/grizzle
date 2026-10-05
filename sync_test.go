package grizzle_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/yourorg/grizzle"
)

func getTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		t.Skipf("skipping integration test; PostgreSQL not available at %s: %v", dsn, err)
	}

	return db
}

func resetPublicSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec("DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;")
	if err != nil {
		t.Fatalf("failed to reset public schema: %v", err)
	}
}

func TestSync_EndToEnd(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	ctx := t.Context()

	// 1. Initial Provisioning
	schemaV1 := `
		CREATE TABLE users (
			id BIGSERIAL PRIMARY KEY,
			email VARCHAR(255) NOT NULL,
			full_name TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`

	err := grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaV1,
		AllowDrop: false,
	})
	if err != nil {
		t.Fatalf("initial Sync failed: %v", err)
	}

	// Insert test data to verify data preservation
	_, err = db.Exec("INSERT INTO users (email, full_name) VALUES ('test@example.com', 'Test User');")
	if err != nil {
		t.Fatalf("failed to insert data: %v", err)
	}

	// 2. Idempotency Check (running the exact same schema again)
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaV1,
		AllowDrop: false,
	})
	if err != nil {
		t.Fatalf("idempotent Sync failed: %v", err)
	}

	// Verify data is still intact
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM users;").Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("expected 1 user, got %d (err: %v)", count, err)
	}

	// 3. Schema Evolution: Add a Column
	schemaV2 := `
		CREATE TABLE users (
			id BIGSERIAL PRIMARY KEY,
			email VARCHAR(255) NOT NULL,
			full_name TEXT NOT NULL,
			is_active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`

	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaV2,
		AllowDrop: false,
	})
	if err != nil {
		t.Fatalf("evolution Sync (add column) failed: %v", err)
	}

	// Verify new column exists and has default value on existing rows
	var isActive bool
	err = db.QueryRow("SELECT is_active FROM users WHERE email = 'test@example.com';").Scan(&isActive)
	if err != nil || !isActive {
		t.Fatalf("expected is_active=true, got %v (err: %v)", isActive, err)
	}

	// 4. Safety Guard: Drop Column with AllowDrop=false
	schemaV3MissingColumn := `
		CREATE TABLE users (
			id BIGSERIAL PRIMARY KEY,
			full_name TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`

	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaV3MissingColumn,
		AllowDrop: false, // Must be blocked!
	})
	if !errors.Is(err, grizzle.ErrDestructiveBlocked) {
		t.Fatalf("expected ErrDestructiveBlocked, got %v", err)
	}

	// Verify email column was NOT dropped
	var email string
	err = db.QueryRow("SELECT email FROM users WHERE id = 1;").Scan(&email)
	if err != nil || email != "test@example.com" {
		t.Fatalf("data loss occurred after rejected destructive change: %v", err)
	}

	// 5. Destructive Drop with AllowDrop=true and AcceptHazards
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL:     schemaV3MissingColumn,
		AllowDrop:     true, // Explicitly allowed
		AcceptHazards: []grizzle.HazardCode{grizzle.HazardDropColumn},
	})
	if err != nil {
		t.Fatalf("Sync with AllowDrop=true failed: %v", err)
	}

	// Verify column was dropped
	err = db.QueryRow("SELECT email FROM users WHERE id = 1;").Scan(&email)
	if err == nil {
		t.Fatalf("expected email column to be dropped, but query succeeded")
	}
}

func TestPlanDiff(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	ctx := t.Context()

	schema := `
		CREATE TABLE teams (
			id BIGSERIAL PRIMARY KEY,
			name VARCHAR(100) NOT NULL
		);
	`

	plan, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		SchemaSQL: schema,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	if len(plan.Steps) != 1 {
		t.Fatalf("expected 1 step in plan, got %d", len(plan.Steps))
	}
	if plan.Steps[0].Type != grizzle.ChangeCreateTable {
		t.Errorf("expected ChangeCreateTable, got %s", plan.Steps[0].Type)
	}

	// Verify table was NOT actually created on the live DB
	var exists bool
	query := "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'teams');"
	err = db.QueryRow(query).Scan(&exists)
	if err != nil || exists {
		t.Fatalf("PlanDiff created table on live DB!")
	}
}

func TestSync_Concurrency(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	schema := `
		CREATE TABLE products (
			id BIGSERIAL PRIMARY KEY,
			sku VARCHAR(50) NOT NULL,
			price NUMERIC NOT NULL
		);
	`

	const numGoroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, numGoroutines)

	for range numGoroutines {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := grizzle.Sync(ctx, db, grizzle.Options{
				SchemaSQL: schema,
				AllowDrop: false,
			})
			if err != nil {
				errs <- err
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Sync failed: %v", err)
	}

	// Verify table exists and is valid
	var exists bool
	query := "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'products');"
	err := db.QueryRow(query).Scan(&exists)
	if err != nil || !exists {
		t.Fatalf("products table not created after concurrent sync")
	}
}

func TestSync_Phase2_Relational(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	ctx := t.Context()

	// 1. Initial schema with Custom Enum, Multi-column Index, Partial Index, and Foreign Key
	schemaV1 := `
		CREATE TYPE account_status AS ENUM ('trial', 'active', 'suspended');

		CREATE TABLE orgs (
			id BIGSERIAL PRIMARY KEY,
			name VARCHAR(100) NOT NULL
		);

		CREATE TABLE accounts (
			id BIGSERIAL PRIMARY KEY,
			org_id BIGINT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			name VARCHAR(100) NOT NULL,
			status account_status NOT NULL DEFAULT 'trial',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE INDEX idx_accounts_name_created ON accounts (name, created_at);
		CREATE UNIQUE INDEX idx_active_accounts_name ON accounts (name) WHERE status = 'active';
	`

	err := grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaV1,
		AllowDrop: false,
	})
	if err != nil {
		t.Fatalf("Phase 2 initial Sync failed: %v", err)
	}

	// Insert data: create org and accounts
	var orgID int64
	err = db.QueryRowContext(ctx, "INSERT INTO orgs (name) VALUES ('Acme Corp') RETURNING id;").Scan(&orgID)
	if err != nil {
		t.Fatalf("failed to insert org: %v", err)
	}

	_, err = db.ExecContext(ctx, "INSERT INTO accounts (org_id, name, status) VALUES ($1, 'Acme Main', 'active');", orgID)
	if err != nil {
		t.Fatalf("failed to insert account: %v", err)
	}

	// Verify partial unique index: inserting duplicate 'Acme Main' with 'active' must fail
	_, err = db.ExecContext(ctx, "INSERT INTO accounts (org_id, name, status) VALUES ($1, 'Acme Main', 'active');", orgID)
	if err == nil {
		t.Fatalf("expected unique index violation on partial index, but insert succeeded")
	}

	// But inserting 'Acme Main' with 'trial' must succeed because partial index only applies to 'active'
	_, err = db.ExecContext(ctx, "INSERT INTO accounts (org_id, name, status) VALUES ($1, 'Acme Main', 'trial');", orgID)
	if err != nil {
		t.Fatalf("inserting trial account with same name should succeed: %v", err)
	}

	// 2. Evolve Enum: Add 'archived' value to account_status and remove idx_accounts_name_created
	schemaV2 := `
		CREATE TYPE account_status AS ENUM ('trial', 'active', 'suspended', 'archived');

		CREATE TABLE orgs (
			id BIGSERIAL PRIMARY KEY,
			name VARCHAR(100) NOT NULL
		);

		CREATE TABLE accounts (
			id BIGSERIAL PRIMARY KEY,
			org_id BIGINT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			name VARCHAR(100) NOT NULL,
			status account_status NOT NULL DEFAULT 'trial',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE UNIQUE INDEX idx_active_accounts_name ON accounts (name) WHERE status = 'active';
	`

	// With AllowDrop: false, dropping idx_accounts_name_created must be blocked!
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaV2,
		AllowDrop: false,
	})
	if !errors.Is(err, grizzle.ErrDestructiveBlocked) {
		t.Fatalf("expected ErrDestructiveBlocked when dropping index with AllowDrop=false, got: %v", err)
	}

	// Now allow drop
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaV2,
		AllowDrop: true,
	})
	if err != nil {
		t.Fatalf("Sync with AllowDrop=true (enum evolution + index drop) failed: %v", err)
	}

	// Verify new enum value 'archived' can now be inserted
	_, err = db.ExecContext(ctx, "INSERT INTO accounts (org_id, name, status) VALUES ($1, 'Acme Archive', 'archived');", orgID)
	if err != nil {
		t.Fatalf("failed to insert row with evolved enum value 'archived': %v", err)
	}

	// 3. Test Foreign Key ON DELETE CASCADE
	_, err = db.ExecContext(ctx, "DELETE FROM orgs WHERE id = $1;", orgID)
	if err != nil {
		t.Fatalf("failed to delete org: %v", err)
	}

	// Verify all accounts belonging to this org were cascaded
	var accountCount int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE org_id = $1;", orgID).Scan(&accountCount)
	if err != nil || accountCount != 0 {
		t.Fatalf("expected 0 accounts after cascade delete, got %d (err: %v)", accountCount, err)
	}
}

func TestSync_Phase3_SafetyAndObservability(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	ctx := t.Context()

	// 1. Initial schema with table, column to drop, and index to drop
	schemaV1 := `
		CREATE TABLE telemetry (
			id BIGSERIAL PRIMARY KEY,
			device_id VARCHAR(50) NOT NULL,
			legacy_code INT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE INDEX idx_telemetry_legacy ON telemetry (legacy_code);
	`

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	err := grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaV1,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("initial Sync failed: %v", err)
	}

	// Verify structured slog entries were written
	logStr := logBuf.String()
	if !strings.Contains(logStr, `"msg":"grizzle: starting schema synchronization"`) {
		t.Errorf("missing starting log entry: %s", logStr)
	}
	if !strings.Contains(logStr, `"msg":"grizzle: synchronization finished successfully"`) {
		t.Errorf("missing finished log entry: %s", logStr)
	}

	// 2. Fine-grained drop permissions:
	// Allow dropping indexes, but FORBID dropping columns!
	schemaV2 := `
		CREATE TABLE telemetry (
			id BIGSERIAL PRIMARY KEY,
			device_id VARCHAR(50) NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`

	allowDropIndex := true
	forbidDropCol := false

	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL:       schemaV2,
		AllowDropIndex:  &allowDropIndex,
		AllowDropColumn: &forbidDropCol,
	})

	if err == nil {
		t.Fatalf("expected error due to forbidden column drop, but Sync succeeded")
	}

	// Verify structured DestructiveViolationError details
	var violationErr *grizzle.DestructiveViolationError
	if !errors.As(err, &violationErr) {
		t.Fatalf("expected *DestructiveViolationError, got: %T (%v)", err, err)
	}
	if !errors.Is(err, grizzle.ErrDestructiveBlocked) {
		t.Fatalf("expected errors.Is(err, ErrDestructiveBlocked) to be true")
	}

	// Only the column drop should be blocked (1 violation), since index drop was permitted!
	if len(violationErr.Violations) != 1 {
		t.Fatalf("expected exactly 1 violation (column drop), got %d: %+v", len(violationErr.Violations), violationErr.Violations)
	}
	if violationErr.Violations[0].Type != grizzle.ChangeDropColumn {
		t.Errorf("expected blocked step to be ChangeDropColumn, got %s", violationErr.Violations[0].Type)
	}
}
