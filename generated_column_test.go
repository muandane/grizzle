package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func TestGeneratedColumn_PureUnit_AddAndDrop(t *testing.T) {
	t.Parallel()

	live := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"products": {
				Name: "products",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "integer", Position: 1},
					"price": {Name: "price", DataType: "numeric", Position: 2},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	desired := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"products": {
				Name: "products",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "integer", Position: 1},
					"price": {Name: "price", DataType: "numeric", Position: 2},
					"tax": {
						Name:       "tax",
						DataType:   "numeric",
						Position:   3,
						Generated: &schema.GeneratedColumn{
							Expr:   "price * 0.2",
							Stored: true,
						},
					},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	// 1. Add generated column
	changes := diff.Diff(live, desired, "public", "_shadow", scope.Filters{})
	if len(changes) != 1 {
		t.Fatalf("expected 1 change adding generated column, got %d", len(changes))
	}
	addCol := changes[0]
	if addCol.Type != plan.ChangeAddColumn || addCol.Column == nil || addCol.Column.Generated == nil {
		t.Fatalf("expected ChangeAddColumn with Generated != nil, got: %+v", addCol)
	}
	if addCol.Column.Generated.Expr != "price * 0.2" || !addCol.Column.Generated.Stored {
		t.Fatalf("unexpected Generated column properties: %+v", addCol.Column.Generated)
	}

	// 2. Drop generated column
	dropChanges := diff.Diff(desired, live, "public", "_shadow", scope.Filters{})
	if len(dropChanges) != 1 {
		t.Fatalf("expected 1 change dropping generated column, got %d", len(dropChanges))
	}
	dropCol := dropChanges[0]
	if dropCol.Type != plan.ChangeDropColumn || dropCol.OldColumn.Name != "tax" {
		t.Fatalf("expected ChangeDropColumn for tax, got: %+v", dropCol)
	}
}

func TestGeneratedColumn_PureUnit_ChangeExprEmitsHazard(t *testing.T) {
	t.Parallel()

	live := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"products": {
				Name: "products",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "integer", Position: 1},
					"price": {Name: "price", DataType: "numeric", Position: 2},
					"tax": {
						Name:     "tax",
						DataType: "numeric",
						Position: 3,
						Generated: &schema.GeneratedColumn{
							Expr:   "(price * 0.20)",
							Stored: true,
						},
					},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	desired := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"products": {
				Name: "products",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "integer", Position: 1},
					"price": {Name: "price", DataType: "numeric", Position: 2},
					"tax": {
						Name:     "tax",
						DataType: "numeric",
						Position: 3,
						Generated: &schema.GeneratedColumn{
							Expr:   "(price * 0.25)",
							Stored: true,
						},
					},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes := diff.Diff(live, desired, "public", "_shadow", scope.Filters{})
	if len(changes) != 1 {
		t.Fatalf("expected 1 change modifying generated expression, got %d", len(changes))
	}

	alterChange := changes[0]
	if alterChange.Type != plan.ChangeAlterColumn {
		t.Fatalf("expected ChangeAlterColumn, got: %s", alterChange.Type)
	}
	if !alterChange.GeneratedChanged {
		t.Fatalf("expected GeneratedChanged to be true on diff.Change")
	}

	step := plan.Step{
		Type:               alterChange.Type,
		Table:              alterChange.Table,
		SQL:                "ALTER TABLE products DROP COLUMN tax; ALTER TABLE products ADD COLUMN tax numeric GENERATED ALWAYS AS (price * 0.25) STORED;",
		IsGeneratedRewrite: true,
	}

	p := &plan.Plan{
		Steps: []plan.Step{step},
	}

	hazards := p.Hazards()
	var foundRewriteHazard bool
	for _, h := range hazards {
		if h.Code == plan.HazardGeneratedRewrite && h.Level == plan.HazardLevelWarning {
			foundRewriteHazard = true
			break
		}
	}
	if !foundRewriteHazard {
		t.Fatalf("expected HazardGeneratedRewrite with WARNING severity, got hazards: %+v", hazards)
	}
}

func TestGeneratedColumn_PureUnit_NoopRoundTrip(t *testing.T) {
	t.Parallel()

	// Live has outer parens (as PG pg_get_expr returns)
	live := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"orders": {
				Name: "orders",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "integer", Position: 1},
					"total": {
						Name:     "total",
						DataType: "integer",
						Position: 2,
						Generated: &schema.GeneratedColumn{
							Expr:   "(qty * unit_price)",
							Stored: true,
						},
					},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	// Desired has normalized formatting without extra parens
	desired := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"orders": {
				Name: "orders",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "integer", Position: 1},
					"total": {
						Name:     "total",
						DataType: "integer",
						Position: 2,
						Generated: &schema.GeneratedColumn{
							Expr:   "qty * unit_price",
							Stored: true,
						},
					},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes := diff.Diff(live, desired, "public", "_shadow", scope.Filters{})
	if len(changes) != 0 {
		t.Fatalf("expected 0 diff changes for identical normalized expression, got %d: %+v", len(changes), changes)
	}
}

func TestGeneratedColumn_SQLite_Parity(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// 1. Initial schema with generated column
	initialSQL := `
		CREATE TABLE items (
			id INTEGER PRIMARY KEY,
			price REAL,
			qty INTEGER,
			total REAL GENERATED ALWAYS AS (price * qty) STORED
		);
	`
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: initialSQL,
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// 2. Round-trip plan should be no-op (0 steps)
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: initialSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("expected 0 steps on round-trip, got %d: %+v", len(p.Steps), p.Steps)
	}

	// 3. Alter expression
	updatedSQL := `
		CREATE TABLE items (
			id INTEGER PRIMARY KEY,
			price REAL,
			qty INTEGER,
			total REAL GENERATED ALWAYS AS (price * qty * 1.1) STORED
		);
	`
	pUpdate, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: updatedSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff on update failed: %v", err)
	}
	var hasRewriteHazard bool
	for _, h := range pUpdate.Hazards() {
		if h.Code == grizzle.HazardGeneratedRewrite {
			hasRewriteHazard = true
			break
		}
	}
	if !hasRewriteHazard {
		t.Fatalf("expected HazardGeneratedRewrite on altered expression, got: %+v", pUpdate.Hazards())
	}
}

func TestGeneratedColumn_Postgres_Integration(t *testing.T) {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://postgres:postgres@localhost:5432/grizzle_test?sslmode=disable"
	}
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres generated column test, db unavailable: %v", err)
	}

	schemaPrefix := fmt.Sprintf("test_gencol_%d", time.Now().UnixNano())
	//nolint:gosec // G201: test creates randomized test schema
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		//nolint:gosec // G201: test cleans up randomized test schema
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// 1. Initial sync with generated stored column
	initialSQL := `
		CREATE TABLE measurements (
			id INT PRIMARY KEY,
			width NUMERIC,
			height NUMERIC,
			area NUMERIC GENERATED ALWAYS AS (width * height) STORED
		);
	`
	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    initialSQL,
	}
	if err := grizzle.Sync(context.Background(), db, opts); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// 2. No-op roundtrip
	p, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("expected 0 steps on no-op roundtrip, got %d: %+v", len(p.Steps), p.Steps)
	}

	// 3. Changing generated expression emits GENERATED_REWRITE hazard
	alteredSQL := `
		CREATE TABLE measurements (
			id INT PRIMARY KEY,
			width NUMERIC,
			height NUMERIC,
			area NUMERIC GENERATED ALWAYS AS (width * height * 1.0) STORED
		);
	`
	opts.SchemaSQL = alteredSQL
	pAltered, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("PlanDiff on altered expression failed: %v", err)
	}
	var hasRewriteHazard bool
	for _, h := range pAltered.Hazards() {
		if h.Code == grizzle.HazardGeneratedRewrite {
			hasRewriteHazard = true
			break
		}
	}
	if !hasRewriteHazard {
		t.Fatalf("expected HazardGeneratedRewrite, got hazards: %+v", pAltered.Hazards())
	}
}
