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
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
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

	changes := diff.Diff(live, desired, "public", "_shadow", scope.Filters{})
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
	err := exec.GateHazards(p, []plan.HazardCode{plan.HazardDropColumn})
	if !errors.Is(err, plan.ErrHazardBlocked) {
		t.Fatalf("expected ErrHazardBlocked when UNMANAGED_DEPENDENCY is not accepted, got: %v", err)
	}

	// Gating: with UNMANAGED_DEPENDENCY accepted, passes
	err = exec.GateHazards(p, []plan.HazardCode{plan.HazardDropColumn, plan.HazardUnmanagedDependency})
	if err != nil {
		t.Fatalf("expected GateHazards to pass when accepted, got: %v", err)
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

	changes := diff.Diff(live, desired, "public", "_shadow", scope.Filters{})
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

	changes := diff.Diff(live, desired, "public", "_shadow", scope.Filters{})
	if len(changes) != 0 {
		t.Fatalf("expected 0 diff changes for unmanaged objects, got %d: %+v", len(changes), changes)
	}
}

func TestUnmanaged_PostgresIntegration(t *testing.T) {
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

