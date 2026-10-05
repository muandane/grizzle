package grizzle_test

import (
	"context"
	"testing"

	"github.com/yourorg/grizzle"
)

func BenchmarkSync_WarmBoot_PostgreSQL(b *testing.B) {
	db := getTestDB(&testing.T{})
	defer func() { _ = db.Close() }()
	resetPublicSchema(&testing.T{}, db)

	schema := `
		CREATE TABLE bench_users (
			id BIGSERIAL PRIMARY KEY,
			email VARCHAR(255) NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE INDEX idx_bench_users_email ON bench_users(email);
	`
	ctx := context.Background()

	// Initial warm-up
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema}); err != nil {
		b.Fatalf("benchmark setup failed: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema}); err != nil {
			b.Fatalf("benchmark sync failed: %v", err)
		}
	}
}

func BenchmarkSync_WarmBoot_SQLite(b *testing.B) {
	db := getSQLiteDB(&testing.T{})
	defer func() { _ = db.Close() }()

	schema := `
		CREATE TABLE bench_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sku TEXT NOT NULL,
			stock INTEGER DEFAULT 0
		);
		CREATE INDEX idx_bench_items_sku ON bench_items(sku);
	`
	ctx := context.Background()

	// Initial warm-up
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema}); err != nil {
		b.Fatalf("benchmark setup failed: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schema}); err != nil {
			b.Fatalf("benchmark sync failed: %v", err)
		}
	}
}
