package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
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

	ctx := context.Background()

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

	// 5. Destructive Drop with AllowDrop=true
	err = grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaV3MissingColumn,
		AllowDrop: true, // Explicitly allowed
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

	ctx := context.Background()

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

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := grizzle.Sync(ctx, db, grizzle.Options{
				SchemaSQL: schema,
				AllowDrop: false,
			})
			if err != nil {
				errs <- err
			}
		}()
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
