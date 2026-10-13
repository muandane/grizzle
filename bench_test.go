package grizzle_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func BenchmarkSync_WarmBoot_PostgreSQL(b *testing.B) {
	b.ReportAllocs()
	db := getTestDB(b)
	resetPublicSchema(b, db)

	schemaSQL := `
		CREATE TABLE bench_users (
			id BIGSERIAL PRIMARY KEY,
			email VARCHAR(255) NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE INDEX idx_bench_users_email ON bench_users(email);
	`
	ctx := context.Background()

	// Initial warm-up
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schemaSQL}); err != nil {
		b.Fatalf("benchmark setup failed: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schemaSQL}); err != nil {
			b.Fatalf("benchmark sync failed: %v", err)
		}
	}
}

func BenchmarkSync_ColdBoot_PostgreSQL(b *testing.B) {
	b.ReportAllocs()
	db := getTestDB(b)

	schemaSQL := `
		CREATE TABLE bench_cold_users (
			id BIGSERIAL PRIMARY KEY,
			email VARCHAR(255) NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE INDEX idx_bench_cold_users_email ON bench_cold_users(email);
	`
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		resetPublicSchema(b, db)
		b.StartTimer()

		if err := grizzle.Sync(ctx, db, grizzle.Options{
			Dialect:   grizzle.DialectPostgres,
			SchemaSQL: schemaSQL,
		}); err != nil {
			b.Fatalf("cold boot sync failed: %v", err)
		}
	}
}

func BenchmarkSync_WarmBoot_SQLite(b *testing.B) {
	b.ReportAllocs()
	db := getSQLiteDB(b)

	schemaSQL := `
		CREATE TABLE bench_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sku TEXT NOT NULL,
			stock INTEGER DEFAULT 0
		);
		CREATE INDEX idx_bench_items_sku ON bench_items(sku);
	`
	ctx := context.Background()

	// Initial warm-up
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schemaSQL}); err != nil {
		b.Fatalf("benchmark setup failed: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: schemaSQL}); err != nil {
			b.Fatalf("benchmark sync failed: %v", err)
		}
	}
}

func BenchmarkSync_ColdBoot_SQLite(b *testing.B) {
	b.ReportAllocs()
	db := getSQLiteDB(b)

	schemaSQL := `
		CREATE TABLE bench_cold_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sku TEXT NOT NULL,
			stock INTEGER DEFAULT 0
		);
		CREATE INDEX idx_bench_cold_items_sku ON bench_cold_items(sku);
	`
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		_, err := db.Exec("DROP TABLE IF EXISTS bench_cold_items; DROP TABLE IF EXISTS grizzle_history;")
		if err != nil {
			b.Fatalf("failed to reset sqlite tables: %v", err)
		}
		b.StartTimer()

		if err := grizzle.Sync(ctx, db, grizzle.Options{
			Dialect:   grizzle.DialectSQLite,
			SchemaSQL: schemaSQL,
		}); err != nil {
			b.Fatalf("cold boot sync failed: %v", err)
		}
	}
}

func BenchmarkPlanDiff_NoDrift_PostgreSQL(b *testing.B) {
	b.ReportAllocs()
	db := getTestDB(b)
	resetPublicSchema(b, db)

	schemaSQL := `
		CREATE TABLE users (
			id BIGSERIAL PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			email VARCHAR(255) NOT NULL UNIQUE
		);
		CREATE TABLE orders (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id),
			amount NUMERIC(10, 2) NOT NULL
		);
	`
	ctx := context.Background()
	opts := grizzle.Options{
		Dialect:   grizzle.DialectPostgres,
		SchemaSQL: schemaSQL,
	}

	if err := grizzle.Sync(ctx, db, opts); err != nil {
		b.Fatalf("warm-up sync failed: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			b.Fatalf("PlanDiff failed: %v", err)
		}
		if len(p.Steps) != 0 {
			b.Fatalf("expected 0 steps on no-drift, got %d", len(p.Steps))
		}
	}
}

func BenchmarkPlanDiff_NewTables_PostgreSQL(b *testing.B) {
	b.ReportAllocs()
	db := getTestDB(b)
	resetPublicSchema(b, db)

	schemaSQL := `
		CREATE TABLE users (
			id BIGSERIAL PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			email VARCHAR(255) NOT NULL UNIQUE
		);
		CREATE TABLE posts (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id),
			title VARCHAR(255) NOT NULL,
			body TEXT
		);
		CREATE TABLE comments (
			id BIGSERIAL PRIMARY KEY,
			post_id BIGINT NOT NULL REFERENCES posts(id),
			content TEXT NOT NULL
		);
	`
	ctx := context.Background()
	opts := grizzle.Options{
		Dialect:   grizzle.DialectPostgres,
		SchemaSQL: schemaSQL,
	}

	b.ResetTimer()
	for b.Loop() {
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			b.Fatalf("PlanDiff failed: %v", err)
		}
		if len(p.Steps) == 0 {
			b.Fatal("expected steps for fresh schema")
		}
	}
}

func BenchmarkPlanDiff_NoDrift_SQLite(b *testing.B) {
	b.ReportAllocs()
	db := getSQLiteDB(b)

	schemaSQL := `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT NOT NULL UNIQUE
		);
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id),
			amount NUMERIC NOT NULL
		);
	`
	ctx := context.Background()
	opts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
	}

	if err := grizzle.Sync(ctx, db, opts); err != nil {
		b.Fatalf("warm-up sync failed: %v", err)
	}

	b.ResetTimer()
	for b.Loop() {
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			b.Fatalf("PlanDiff failed: %v", err)
		}
		if len(p.Steps) != 0 {
			b.Fatalf("expected 0 steps on no-drift, got %d", len(p.Steps))
		}
	}
}

func BenchmarkPlanDiff_NewTables_SQLite(b *testing.B) {
	b.ReportAllocs()
	db := getSQLiteDB(b)

	schemaSQL := `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT NOT NULL UNIQUE
		);
		CREATE TABLE posts (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id),
			title TEXT NOT NULL,
			body TEXT
		);
		CREATE TABLE comments (
			id INTEGER PRIMARY KEY,
			post_id INTEGER NOT NULL REFERENCES posts(id),
			content TEXT NOT NULL
		);
	`
	ctx := context.Background()
	opts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
	}

	b.ResetTimer()
	for b.Loop() {
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			b.Fatalf("PlanDiff failed: %v", err)
		}
		if len(p.Steps) == 0 {
			b.Fatal("expected steps for fresh schema")
		}
	}
}

func BenchmarkPlan_Hash(b *testing.B) {
	b.ReportAllocs()
	steps := make([]grizzle.Step, 50)
	for i := range steps {
		steps[i] = grizzle.Step{
			Type:        grizzle.ChangeCreateTable,
			Table:       fmt.Sprintf("table_%d", i),
			SQL:         fmt.Sprintf("CREATE TABLE table_%d (id INT PRIMARY KEY, val TEXT);", i),
			Destructive: false,
		}
	}

	p := &grizzle.Plan{
		TargetSchema:   "public",
		TargetSchemas:  []string{"public", "analytics"},
		Steps:          steps,
		IncludeTables:  []string{"table_1", "table_2", "table_3"},
		ExcludeTables:  []string{"audit_*"},
		Renames:        map[string]string{"table_1.val": "value", "table_2.val": "data"},
		ExpandContract: true,
		SchemaSQL:      "CREATE TABLE table_0 (id INT);",
	}

	b.ResetTimer()
	for b.Loop() {
		_ = p.Hash()
	}
}

func BenchmarkNormalizeExpression(b *testing.B) {
	b.ReportAllocs()
	expr := `((((("status")::text = 'active'::character varying::text) AND (amount > (0)::numeric)) IS TRUE))`

	b.ResetTimer()
	for b.Loop() {
		_ = schema.NormalizeExpression(expr, nil)
	}
}

func BenchmarkNormalizeCheckDef(b *testing.B) {
	b.ReportAllocs()
	checkDef := `CHECK (((price > (0)::numeric) AND (status::text = 'valid'::text))) NOT VALID`

	b.ResetTimer()
	for b.Loop() {
		_ = schema.NormalizeCheckDef(checkDef, nil)
	}
}

func BenchmarkSortSteps(b *testing.B) {
	b.ReportAllocs()
	steps := make([]plan.Step, 100)
	for i := range steps {
		steps[i] = plan.Step{
			Type:  plan.ChangeCreateTable,
			Table: fmt.Sprintf("tbl_%d", i),
			SQL:   fmt.Sprintf("CREATE TABLE tbl_%d (id INT);", i),
		}
		if i > 0 {
			steps[i].DependsOn = []string{fmt.Sprintf("tbl_%d", i-1)}
		}
	}

	b.ResetTimer()
	for b.Loop() {
		cloned := make([]plan.Step, len(steps))
		copy(cloned, steps)
		plan.SortSteps(cloned)
	}
}
