package grizzle_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

var updateGolden = flag.Bool("update", false, "update golden files")

func TestPlan_Hash_Stability(t *testing.T) {
	t.Parallel()

	p1 := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{Type: grizzle.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users (id int);"},
			{Type: grizzle.ChangeAddColumn, Table: "users", SQL: "ALTER TABLE users ADD COLUMN age int NOT NULL;", ColumnNotNull: true},
		},
		IncludeTables: []string{"users", "posts"},
		ExcludeTables: []string{"audit_*"},
	}

	hash1 := p1.Hash()
	if hash1 == "" {
		t.Fatal("expected non-empty plan hash")
	}

	// 1. Verify 100 consecutive runs yield identical hash
	for i := range 100 {
		if p1.Hash() != hash1 {
			t.Fatalf("hash non-deterministic on iteration %d: got %s, want %s", i, p1.Hash(), hash1)
		}
	}

	// 2. Scope list permutation should produce identical hash due to canonical sorting
	p2 := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{Type: grizzle.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users (id int);"},
			{Type: grizzle.ChangeAddColumn, Table: "users", SQL: "ALTER TABLE users ADD COLUMN age int NOT NULL;", ColumnNotNull: true},
		},
		IncludeTables: []string{"posts", "users"}, // reversed order
		ExcludeTables: []string{"audit_*"},
	}
	if p2.Hash() != hash1 {
		t.Errorf("expected permutation of IncludeTables to yield identical canonical hash: got %s, want %s", p2.Hash(), hash1)
	}
}

func TestPlan_Hash_Sensitivity(t *testing.T) {
	t.Parallel()

	base := func() *grizzle.Plan {
		return &grizzle.Plan{
			TargetSchema: "public",
			Steps: []grizzle.Step{
				{Type: grizzle.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users (id int);"},
			},
			IncludeTables: []string{"users"},
		}
	}

	baseHash := base().Hash()

	variants := map[string]*grizzle.Plan{
		"target_schema changed": {
			TargetSchema:  "other",
			Steps:         base().Steps,
			IncludeTables: base().IncludeTables,
		},
		"step type changed": {
			TargetSchema: base().TargetSchema,
			Steps: []grizzle.Step{
				{Type: grizzle.ChangeDropTable, Table: "users", SQL: "CREATE TABLE users (id int);"},
			},
			IncludeTables: base().IncludeTables,
		},
		"step table changed": {
			TargetSchema: base().TargetSchema,
			Steps: []grizzle.Step{
				{Type: grizzle.ChangeCreateTable, Table: "accounts", SQL: "CREATE TABLE users (id int);"},
			},
			IncludeTables: base().IncludeTables,
		},
		"step SQL changed": {
			TargetSchema: base().TargetSchema,
			Steps: []grizzle.Step{
				{Type: grizzle.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users (id bigint);"},
			},
			IncludeTables: base().IncludeTables,
		},
		"destructive flag changed": {
			TargetSchema: base().TargetSchema,
			Steps: []grizzle.Step{
				{Type: grizzle.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users (id int);", Destructive: true},
			},
			IncludeTables: base().IncludeTables,
		},
		"column_not_null flag changed": {
			TargetSchema: base().TargetSchema,
			Steps: []grizzle.Step{
				{Type: grizzle.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users (id int);", ColumnNotNull: true},
			},
			IncludeTables: base().IncludeTables,
		},
		"type_narrowed flag changed": {
			TargetSchema: base().TargetSchema,
			Steps: []grizzle.Step{
				{Type: grizzle.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users (id int);", TypeNarrowed: true},
			},
			IncludeTables: base().IncludeTables,
		},
		"include_tables changed": {
			TargetSchema:  base().TargetSchema,
			Steps:         base().Steps,
			IncludeTables: []string{"users", "extra"},
		},
		"exclude_tables changed": {
			TargetSchema:  base().TargetSchema,
			Steps:         base().Steps,
			IncludeTables: base().IncludeTables,
			ExcludeTables: []string{"legacy_*"},
		},
	}

	for name, variant := range variants {
		vHash := variant.Hash()
		if vHash == baseHash {
			t.Errorf("expected hash to change for variant %q, but matched base hash %s", name, baseHash)
		}
	}
}

func TestPlan_JSON_Roundtrip(t *testing.T) {
	t.Parallel()

	original := &grizzle.Plan{
		TargetSchema: "public",
		Steps: []grizzle.Step{
			{
				Type:             grizzle.ChangeCreateTable,
				Table:            "users",
				SQL:              "CREATE TABLE users (id int);",
				Destructive:      false,
				ColumnNotNull:    true,
				ColumnHasDefault: false,
			},
			{
				Type:             grizzle.ChangeAlterColumn,
				Table:            "users",
				SQL:              "ALTER TABLE users ALTER COLUMN id TYPE smallint;",
				Destructive:      true,
				TypeNarrowed:     true,
				ColumnNotNull:    true,
				ColumnHasDefault: false,
			},
		},
		IncludeTables: []string{"users"},
		ExcludeTables: []string{"temp_*"},
		SchemaSQL:     "CREATE TABLE users (id int);",
	}

	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var parsed grizzle.Plan
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if parsed.Hash() != original.Hash() {
		t.Errorf("hash mismatch after JSON roundtrip:\noriginal: %s\nparsed:   %s", original.Hash(), parsed.Hash())
	}
}

func TestApply_ExpectedHash(t *testing.T) {
	ctx := context.Background()
	db := setupSQLiteDB(t)

	initialSQL := "CREATE TABLE users (id INTEGER PRIMARY KEY);"
	if err := grizzle.Sync(ctx, db, grizzle.Options{SchemaSQL: initialSQL}); err != nil {
		t.Fatalf("initial setup failed: %v", err)
	}

	desiredSQL := "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);"
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{SchemaSQL: desiredSQL})
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	approvedHash := p.Hash()

	// 1. Apply with wrong ExpectedHash aborts with ErrPlanDrift
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{
		ExpectedHash: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if !errors.Is(err, grizzle.ErrPlanDrift) {
		t.Fatalf("expected ErrPlanDrift on hash mismatch, got: %v", err)
	}

	// 2. Apply with matching ExpectedHash succeeds
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{
		ExpectedHash: approvedHash,
	})
	if err != nil {
		t.Fatalf("expected Apply to succeed with approved hash, got: %v", err)
	}

	// Verify column exists
	var email sql.NullString
	if err := db.QueryRow("SELECT email FROM users LIMIT 1;").Scan(&email); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed querying email column after apply: %v", err)
	}

	// 3. Applying the same plan again now detects drift because database is already updated!
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{
		ExpectedHash: approvedHash,
	})
	if !errors.Is(err, grizzle.ErrPlanDrift) {
		t.Fatalf("expected ErrPlanDrift because live schema already changed, got: %v", err)
	}
}

func TestPlan_GoldenFile(t *testing.T) {
	schemaBytes, err := os.ReadFile(filepath.Join("testdata", "sample.sql"))
	if err != nil {
		t.Fatalf("failed reading testdata/sample.sql: %v", err)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: string(schemaBytes),
	})
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	goldenPath := filepath.Join("testdata", "plan.golden.json")
	planJSON, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	if *updateGolden {
		if err := os.WriteFile(goldenPath, planJSON, 0600); err != nil {
			t.Fatalf("failed updating golden file: %v", err)
		}
		t.Logf("Updated golden file %s", goldenPath)
	}

	expectedJSON, err := os.ReadFile(goldenPath) //nolint:gosec // G304: test reads static testdata golden file
	if os.IsNotExist(err) {
		// First-time generation
		if err := os.WriteFile(goldenPath, planJSON, 0600); err != nil {
			t.Fatalf("failed writing initial golden file: %v", err)
		}
		expectedJSON = planJSON
	} else if err != nil {
		t.Fatalf("failed reading golden file: %v", err)
	}

	var goldenPlan grizzle.Plan
	if err := json.Unmarshal(expectedJSON, &goldenPlan); err != nil {
		t.Fatalf("failed parsing golden plan: %v", err)
	}

	if p.Hash() != goldenPlan.Hash() {
		t.Errorf("plan hash mismatch against golden file:\ngot:  %s\nwant: %s", p.Hash(), goldenPlan.Hash())
	}
}

func TestPlan_Hash_ExcludesOperationalFields(t *testing.T) {
	ctx := context.Background()
	db := setupSQLiteDB(t)

	schemaSQL := `CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT);`

	baseOpts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
	}

	basePlan, err := grizzle.PlanDiff(ctx, db, baseOpts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	baseHash := basePlan.Hash()

	t.Run("excluded_AcceptHazards", func(t *testing.T) {
		opts := baseOpts
		opts.AcceptHazards = []grizzle.HazardCode{grizzle.HazardDropTable, grizzle.HazardDropColumn}
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			t.Fatalf("PlanDiff failed: %v", err)
		}
		if p.Hash() != baseHash {
			t.Errorf("AcceptHazards altered hash: got %s, want %s", p.Hash(), baseHash)
		}
	})

	t.Run("excluded_LockTimeout", func(t *testing.T) {
		opts := baseOpts
		opts.LockTimeout = 42 * time.Second
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			t.Fatalf("PlanDiff failed: %v", err)
		}
		if p.Hash() != baseHash {
			t.Errorf("LockTimeout altered hash: got %s, want %s", p.Hash(), baseHash)
		}
	})

	t.Run("excluded_StatementTimeout", func(t *testing.T) {
		opts := baseOpts
		opts.StatementTimeout = 15 * time.Minute
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			t.Fatalf("PlanDiff failed: %v", err)
		}
		if p.Hash() != baseHash {
			t.Errorf("StatementTimeout altered hash: got %s, want %s", p.Hash(), baseHash)
		}
	})

	t.Run("excluded_MaxRetries", func(t *testing.T) {
		opts := baseOpts
		opts.MaxRetries = 99
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			t.Fatalf("PlanDiff failed: %v", err)
		}
		if p.Hash() != baseHash {
			t.Errorf("MaxRetries altered hash: got %s, want %s", p.Hash(), baseHash)
		}
	})

	t.Run("excluded_Timestamps", func(t *testing.T) {
		// Calling Hash() across time intervals must produce strictly identical digest
		h1 := basePlan.Hash()
		time.Sleep(50 * time.Millisecond)
		h2 := basePlan.Hash()
		if h1 != h2 {
			t.Errorf("Timestamp difference altered hash: %s vs %s", h1, h2)
		}
	})

	t.Run("included_Renames", func(t *testing.T) {
		pWithRenames := *basePlan
		pWithRenames.Renames = map[string]string{"customers.name": "full_name"}
		if pWithRenames.Hash() == baseHash {
			t.Errorf("expected Renames to alter hash, but remained identical")
		}
	})
}
