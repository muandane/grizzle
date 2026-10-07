package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestUnmanaged_HazardOnDropColumn_PureUnit(t *testing.T) {
	t.Parallel()

	live := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "integer", Position: 1},
					"email": {Name: "email", DataType: "text", Position: 2},
					"name":  {Name: "name", DataType: "text", Position: 3},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
		Unmanaged: map[string]*schema.UnmanagedObject{
			"v_user_emails": {
				Name: "v_user_emails",
				Kind: schema.UnmanagedView,
				DependsOn: []schema.DependencyRef{
					{Table: "users", Column: "email"},
				},
			},
		},
	}

	desired := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":   {Name: "id", DataType: "integer", Position: 1},
					"name": {Name: "name", DataType: "text", Position: 2},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(live, desired, "public", "_shadow", scope.Filters{})
	if err != nil {
		t.Fatalf("unexpected diff error: %v", err)
	}
	var dropCol *diff.Change
	for i := range changes {
		if changes[i].Type == plan.ChangeDropColumn && changes[i].OldColumn != nil && changes[i].OldColumn.Name == "email" {
			dropCol = &changes[i]
			break
		}
	}
	if dropCol == nil {
		t.Fatalf("expected ChangeDropColumn for email")
	}
	if len(dropCol.UnmanagedDeps) == 0 {
		t.Fatalf("expected UnmanagedDeps to be populated on dropCol Change")
	}

	step := plan.Step{
		Type:          dropCol.Type,
		Table:         dropCol.Table,
		SQL:           "ALTER TABLE users DROP COLUMN email;",
		Destructive:   true,
		UnmanagedDeps: dropCol.UnmanagedDeps,
	}

	p := &plan.Plan{
		Steps:  []plan.Step{step},
		Policy: plan.DropPolicy{AllowColumn: true},
	}

	hazards := p.Hazards()
	var foundHazard bool
	for _, h := range hazards {
		if h.Code == plan.HazardUnmanagedDependency && h.Level == plan.HazardLevelCritical {
			foundHazard = true
			break
		}
	}
	if !foundHazard {
		t.Fatalf("expected HazardUnmanagedDependency with CRITICAL level, got: %+v", hazards)
	}

	// Gating: without accepting UNMANAGED_DEPENDENCY, execution must be blocked
	err = p.ValidateHazards([]plan.HazardCode{plan.HazardDropColumn})
	if !errors.Is(err, plan.ErrHazardBlocked) {
		t.Fatalf("expected ErrHazardBlocked when UNMANAGED_DEPENDENCY is not accepted, got: %v", err)
	}

	// Gating: with UNMANAGED_DEPENDENCY accepted, passes
	err = p.ValidateHazards([]plan.HazardCode{plan.HazardDropColumn, plan.HazardUnmanagedDependency})
	if err != nil {
		t.Fatalf("expected ValidateHazards to pass when accepted, got: %v", err)
	}
}

func TestUnmanaged_UnrelatedViewIgnored_PureUnit(t *testing.T) {
	t.Parallel()

	live := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "integer", Position: 1},
					"email": {Name: "email", DataType: "text", Position: 2},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
		Unmanaged: map[string]*schema.UnmanagedObject{
			"v_other": {
				Name: "v_other",
				Kind: schema.UnmanagedView,
				DependsOn: []schema.DependencyRef{
					{Table: "other_table", Column: "data"},
				},
			},
		},
	}

	desired := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "integer", Position: 1},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(live, desired, "public", "_shadow", scope.Filters{})
	if err != nil {
		t.Fatalf("unexpected diff error: %v", err)
	}
	for _, c := range changes {
		if len(c.UnmanagedDeps) > 0 {
			t.Fatalf("expected 0 UnmanagedDeps for unrelated view, got: %+v", c.UnmanagedDeps)
		}
	}
}

func TestUnmanaged_NeverDiffDropped_PureUnit(t *testing.T) {
	t.Parallel()

	live := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "integer", Position: 1},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
		Unmanaged: map[string]*schema.UnmanagedObject{
			"v_users": {
				Name: "v_users",
				Kind: schema.UnmanagedView,
				DependsOn: []schema.DependencyRef{
					{Table: "users", Column: "id"},
				},
			},
			"trg_audit": {
				Name:  "trg_audit",
				Kind:  schema.UnmanagedTrigger,
				Table: "users",
			},
			"calc_total": {
				Name: "calc_total",
				Kind: schema.UnmanagedFunction,
			},
		},
	}

	desired := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "integer", Position: 1},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(live, desired, "public", "_shadow", scope.Filters{})
	if err != nil {
		t.Fatalf("unexpected diff error: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected 0 diff changes for unmanaged objects, got %d: %+v", len(changes), changes)
	}
}

func TestUnmanaged_PostgresIntegration(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres unmanaged object test, db unavailable: %v", err)
	}

	schemaPrefix := fmt.Sprintf("test_unm_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// 1. Setup table + unmanaged view + unmanaged trigger + unrelated view
	//nolint:gosec // G201: test constructs setup DDL with randomized schema prefix
	setupSQL := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE TABLE accounts (id INT PRIMARY KEY, email TEXT, balance NUMERIC);
		CREATE VIEW v_account_emails AS SELECT id, email FROM accounts;
		CREATE VIEW v_unrelated AS SELECT 1 AS num;
		CREATE OR REPLACE FUNCTION trg_noop_fn() RETURNS trigger AS $$
		BEGIN
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_audit BEFORE INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION trg_noop_fn();
	`, schemaPrefix)
	if _, err := db.Exec(setupSQL); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// 2. Desired schema drops email
	desiredSQL := `
		CREATE TABLE accounts (
			id INT PRIMARY KEY,
			balance NUMERIC
		);
	`

	opts := grizzle.Options{
		Dialect:        grizzle.DialectPostgres,
		TargetSchema:   schemaPrefix,
		SchemaSQL:      desiredSQL,
		AllowDropTable: new(true),
	}

	// 3. PlanDiff should detect the hazard UNMANAGED_DEPENDENCY
	p, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	var hasUnmanagedHazard bool
	for _, h := range p.Hazards() {
		if h.Code == grizzle.HazardUnmanagedDependency {
			hasUnmanagedHazard = true
			break
		}
	}
	if !hasUnmanagedHazard {
		t.Fatalf("expected HazardUnmanagedDependency in plan hazards, got: %+v", p.Hazards())
	}

	// 4. Sync without accepting hazard must fail
	opts.AllowDropColumn = new(true)
	err = grizzle.Sync(context.Background(), db, opts)
	if !errors.Is(err, grizzle.ErrHazardBlocked) {
		t.Fatalf("expected ErrHazardBlocked on Sync without AcceptHazards, got: %v", err)
	}

	// 5. Trigger on accounts must still be present
	var trgCount int
	err = db.QueryRow(`
		SELECT count(*)
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = 'accounts' AND t.tgname = 'trg_audit';
	`, schemaPrefix).Scan(&trgCount)
	if err != nil {
		t.Fatalf("checking trigger failed: %v", err)
	}
	if trgCount != 1 {
		t.Fatalf("expected trigger trg_audit to still exist, found count=%d", trgCount)
	}
}

func TestUnmanaged_FunctionDependency_PostgresIntegration(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres function dependency test, db unavailable: %v", err)
	}

	schemaPrefix := fmt.Sprintf("test_unmfn_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// 1. Setup: managed table + two unmanaged functions referencing it.
	//    f_atomic uses a SQL-standard body (BEGIN ATOMIC, PG14+): PostgreSQL
	//    records exact table AND column dependencies in pg_depend.
	//    f_string uses a quoted plpgsql body: no pg_depend entries exist, so
	//    Grizzle must discover the dependency by scanning the source text.
	//nolint:gosec // G201: test constructs setup DDL with randomized schema prefix
	setupSQL := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE TABLE orders (id INT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'new');
		CREATE FUNCTION f_atomic_ref() RETURNS text LANGUAGE SQL BEGIN ATOMIC
			SELECT status FROM orders WHERE id = 1;
		END;
		CREATE FUNCTION f_string_ref() RETURNS text AS $$
		BEGIN
			RETURN (SELECT status FROM orders WHERE id = 2);
		END;
		$$ LANGUAGE plpgsql;
	`, schemaPrefix)
	if _, err := db.Exec(setupSQL); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// 2. Desired schema drops the status column both functions reference.
	desiredSQL := `
		CREATE TABLE orders (
			id INT PRIMARY KEY
		);
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQL,
	}

	p, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// 3. Both functions must be introspected with dependencies on orders.
	ir, err := postgres.Inspect(context.Background(), db, schemaPrefix)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}
	for _, fnName := range []string{"f_atomic_ref", "f_string_ref"} {
		obj, ok := ir.Unmanaged["function:"+fnName]
		if !ok {
			t.Fatalf("expected unmanaged function %s to be introspected, got: %v", fnName, ir.Unmanaged)
		}
		if len(obj.DependsOn) == 0 {
			t.Fatalf("expected %s DependsOn to be populated (pg_depend or source scan)", fnName)
		}
		foundOrders := false
		for _, ref := range obj.DependsOn {
			if ref.Table == "orders" {
				foundOrders = true
			}
		}
		if !foundOrders {
			t.Fatalf("expected %s to depend on table orders, got: %+v", fnName, obj.DependsOn)
		}
	}

	// 4. The drop-column step must carry UNMANAGED_DEPENDENCY for both functions.
	var hasUnmanagedHazard bool
	for _, h := range p.Hazards() {
		if h.Code == grizzle.HazardUnmanagedDependency {
			hasUnmanagedHazard = true
			break
		}
	}
	if !hasUnmanagedHazard {
		t.Fatalf("expected HazardUnmanagedDependency for function dependency, got: %+v", p.Hazards())
	}

	// 5. Sync without accepting the hazard must be blocked.
	opts.AllowDropColumn = new(true)
	err = grizzle.Sync(context.Background(), db, opts)
	if !errors.Is(err, grizzle.ErrHazardBlocked) {
		t.Fatalf("expected ErrHazardBlocked on Sync without AcceptHazards, got: %v", err)
	}

	// 6. Sync with UNMANAGED_DEPENDENCY accepted proceeds and drops the column.
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:         grizzle.DialectPostgres,
		TargetSchema:    schemaPrefix,
		SchemaSQL:       desiredSQL,
		AllowDropColumn: new(true),
		AcceptHazards:   []grizzle.HazardCode{grizzle.HazardUnmanagedDependency, grizzle.HazardDropColumn},
	})
	if err != nil {
		t.Fatalf("expected Sync to succeed with hazard accepted, got: %v", err)
	}
	var colCount int
	err = db.QueryRow(
		"SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'orders' AND column_name = 'status';",
		schemaPrefix).Scan(&colCount)
	if err != nil {
		t.Fatalf("checking column failed: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("expected status column to be dropped, found count=%d", colCount)
	}
}
