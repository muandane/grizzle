package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/muandane/grizzle"
)

func TestScopeSafety_StrictScopeRequiresIncludeTables(t *testing.T) {
	opts := grizzle.Options{
		Dialect:     grizzle.DialectSQLite,
		SchemaSQL:   "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		StrictScope: true,
		// IncludeTables is empty
	}

	err := opts.Validate()
	if !errors.Is(err, grizzle.ErrStrictScope) {
		t.Fatalf("expected ErrStrictScope on Validate(), got: %v", err)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	err = grizzle.Sync(ctx, db, opts)
	if !errors.Is(err, grizzle.ErrStrictScope) {
		t.Fatalf("expected ErrStrictScope on Sync(), got: %v", err)
	}

	_, err = grizzle.PlanDiff(ctx, db, opts)
	if !errors.Is(err, grizzle.ErrStrictScope) {
		t.Fatalf("expected ErrStrictScope on PlanDiff(), got: %v", err)
	}
}

func TestScopeSafety_StrictScopeWithIncludeTablesSucceeds(t *testing.T) {
	opts := grizzle.Options{
		Dialect:       grizzle.DialectSQLite,
		SchemaSQL:     "CREATE TABLE users (id INTEGER PRIMARY KEY);",
		StrictScope:   true,
		IncludeTables: []string{"users"},
	}

	if err := opts.Validate(); err != nil {
		t.Fatalf("expected valid options, got: %v", err)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	err = grizzle.Sync(ctx, db, opts)
	if err != nil {
		t.Fatalf("expected successful Sync with IncludeTables, got: %v", err)
	}

	p, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("expected successful PlanDiff, got: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Errorf("expected 0 steps after sync, got %d", len(p.Steps))
	}
}

func TestScopeSafety_BuiltinExtensionExclusions(t *testing.T) {
	connStr := os.Getenv("POSTGRES_DSN")
	if connStr == "" {
		connStr = "postgres://postgres:postgres@localhost:5432/grizzle_test?sslmode=disable"
	}
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres scope test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_scope_ext_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	// Simulate postgis and metadata tables existing in the schema
	mockTablesSQL := fmt.Sprintf(`
		CREATE TABLE %s.spatial_ref_sys (srid INTEGER PRIMARY KEY, auth_name TEXT);
		CREATE TABLE %s.geometry_columns (f_table_name TEXT PRIMARY KEY);
		CREATE TABLE %s.grizzle_history (id BIGINT PRIMARY KEY);
		CREATE TABLE %s.users (id BIGINT PRIMARY KEY);
	`, schema, schema, schema, schema)
	if _, err := db.Exec(mockTablesSQL); err != nil {
		t.Fatalf("failed creating mock tables: %v", err)
	}

	// User schema only specifies users table
	userSchema := `CREATE TABLE users (id BIGINT PRIMARY KEY);`

	// Even with AllowDrop=true, built-in extension tables must NEVER be dropped
	plan, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    userSchema,
		AllowDrop:    true,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	for _, s := range plan.Steps {
		if s.Type == grizzle.ChangeDropTable {
			if s.Table == "spatial_ref_sys" || s.Table == "geometry_columns" || s.Table == "grizzle_history" {
				t.Fatalf("forbidden drop step planned for built-in protected table: %+v", s)
			}
		}
	}
}
