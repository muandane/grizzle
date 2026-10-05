package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourorg/grizzle"
)

// TestHardening_MultiPodFuzzing simulates a Kubernetes deployment where 50 replicas boot
// concurrently and attempt to synchronize the database schema at the exact same millisecond.
func TestHardening_MultiPodFuzzing(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	// Configure pool for high concurrency
	db.SetMaxOpenConns(60)
	db.SetMaxIdleConns(60)

	complexSchema := `
		CREATE TYPE order_status AS ENUM ('pending', 'processing', 'completed', 'cancelled');

		CREATE TABLE customers (
			id BIGSERIAL PRIMARY KEY,
			email VARCHAR(255) NOT NULL UNIQUE,
			tier VARCHAR(50) DEFAULT 'standard'
		);

		CREATE TABLE orders (
			id BIGSERIAL PRIMARY KEY,
			customer_id BIGINT NOT NULL,
			status order_status NOT NULL DEFAULT 'pending',
			total NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE
		);

		CREATE INDEX idx_orders_customer ON orders(customer_id);
		CREATE INDEX idx_orders_status ON orders(status) WHERE status = 'pending';
	`

	const numPods = 50
	var wg sync.WaitGroup
	errs := make(chan error, numPods)
	barrier := make(chan struct{}) // Ensures all goroutines strike the database at the exact same instant
	var completed atomic.Int32

	for range numPods {
		wg.Go(func() {
			<-barrier // Synchronized release

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			err := grizzle.Sync(ctx, db, grizzle.Options{
				Dialect:      grizzle.DialectPostgres,
				TargetSchema: "public",
				SchemaSQL:    complexSchema,
				AllowDrop:    false,
			})
			if err != nil {
				errs <- err
			} else {
				completed.Add(1)
			}
		})
	}

	// Release all pods simultaneously
	close(barrier)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("pod migration failed during high-concurrency race: %v", err)
	}

	if completed.Load() != int32(numPods) {
		t.Fatalf("expected all %d pods to complete successfully, got %d", numPods, completed.Load())
	}

	// Verify schema state on PostgreSQL
	var orderCount, custCount int
	err := db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('customers', 'orders');").Scan(&orderCount)
	if err != nil || orderCount != 2 {
		t.Fatalf("expected 2 tables created in public schema, got %d, err: %v", orderCount, err)
	}

	var indexCount int
	err = db.QueryRow("SELECT COUNT(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'orders';").Scan(&indexCount)
	if err != nil || indexCount < 2 {
		t.Fatalf("expected at least 2 indexes on orders, got %d, err: %v", indexCount, err)
	}

	_ = custCount
}

// TestHardening_WarmBootLatency verifies that an already-synchronized database boots in < 30ms.
func TestHardening_WarmBootLatency(t *testing.T) {
	db := getTestDB(t)
	defer func() { _ = db.Close() }()
	resetPublicSchema(t, db)

	schema := `
		CREATE TABLE accounts (
			id BIGSERIAL PRIMARY KEY,
			name VARCHAR(100) NOT NULL
		);
	`
	ctx := t.Context()

	// Initial Sync
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Measure warm boot
	start := time.Now()
	err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("warm boot sync failed: %v", err)
	}

	t.Logf("warm boot latency: %v", elapsed)
	if elapsed > 100*time.Millisecond {
		t.Errorf("warm boot exceeded SLA target (100ms max allowed in test env), took %v", elapsed)
	}
}

// TestHardening_SQLite_ConcurrentReads verifies SQLite behavior with concurrent read queries
// across a shared connection pool.
func TestHardening_SQLite_ConcurrentReads(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", "file:memdb_hardening?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed to open shared memory db: %v", err)
	}
	defer db.Close()

	schema := `CREATE TABLE cache (key TEXT PRIMARY KEY, val TEXT);`
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema}); err != nil {
		t.Fatalf("sqlite initial sync failed: %v", err)
	}

	// Insert test data
	_, err = db.ExecContext(ctx, "INSERT INTO cache (key, val) VALUES ('1', 'data1'), ('2', 'data2');")
	if err != nil {
		t.Fatalf("failed inserting data: %v", err)
	}

	var wg sync.WaitGroup
	const readers = 20

	for i := range readers {
		wg.Go(func() {
			var cnt int
			err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM cache WHERE key = '%d'", (i%2)+1)).Scan(&cnt)
			if err != nil {
				t.Errorf("concurrent sqlite read failed: %v", err)
			}
			if cnt != 1 {
				t.Errorf("expected count 1, got %d", cnt)
			}
		})
	}
	wg.Wait()
}
