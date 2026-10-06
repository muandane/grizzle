package grizzle_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/plan"
)

func TestMultiSchema_PostgresRoundTrip(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	ts := time.Now().UnixNano()
	s1 := fmt.Sprintf("ms_ident_%d", ts)
	s2 := fmt.Sprintf("ms_bill_%d", ts)

	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s1))
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s2))
	}()

	schemaSQL := fmt.Sprintf(`
		CREATE TABLE %q.users (
			id INT PRIMARY KEY,
			username TEXT NOT NULL
		);

		CREATE TABLE %q.accounts (
			account_id INT PRIMARY KEY,
			user_id INT NOT NULL REFERENCES %q.users(id),
			balance INT NOT NULL DEFAULT 0
		);
	`, s1, s2, s1)

	ctx := context.Background()

	// 1. Initial Sync
	err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     schemaSQL,
	})
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Verify live tables and cross-schema FK in catalog
	s1Schema, err := postgres.Inspect(ctx, db, s1)
	if err != nil {
		t.Fatalf("Inspect s1 failed: %v", err)
	}
	if _, ok := s1Schema.Tables["users"]; !ok {
		t.Fatalf("expected table users in %s", s1)
	}

	s2Schema, err := postgres.Inspect(ctx, db, s2)
	if err != nil {
		t.Fatalf("Inspect s2 failed: %v", err)
	}
	tblAccounts, ok := s2Schema.Tables["accounts"]
	if !ok {
		t.Fatalf("expected table accounts in %s", s2)
	}
	if len(tblAccounts.ForeignKeys) == 0 {
		t.Fatalf("expected foreign key on accounts in %s", s2)
	}

	// 2. Round-trip idempotency: PlanDiff must be completely empty
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     schemaSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(p.Steps) != 0 {
		for i, s := range p.Steps {
			t.Logf("unexpected step %d: %s (%s)", i, s.Type, s.SQL)
		}
		t.Fatalf("expected 0 diff steps on round-trip, got %d", len(p.Steps))
	}

	// 3. Check must report nil (no drift)
	if err := grizzle.Check(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     schemaSQL,
	}); err != nil {
		t.Errorf("Check returned unexpected error: %v", err)
	}
}

func TestMultiSchema_CrossSchemaFKToposortAndDrop(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	ts := time.Now().UnixNano()
	s1 := fmt.Sprintf("ms_user_%d", ts)
	s2 := fmt.Sprintf("ms_order_%d", ts)

	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s1))
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", s2))
	}()

	schemaSQL := fmt.Sprintf(`
		CREATE TABLE %q.users (
			id INT PRIMARY KEY,
			email TEXT NOT NULL
		);

		CREATE TABLE %q.orders (
			id INT PRIMARY KEY,
			user_id INT NOT NULL REFERENCES %q.users(id)
		);
	`, s1, s2, s1)

	ctx := context.Background()

	// 1. PlanDiff to verify creation topological order: users before orders
	p, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     schemaSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	userCreateIdx, orderCreateIdx := -1, -1
	for i, step := range p.Steps {
		if step.Type == plan.ChangeCreateTable && step.Table == "users" && step.Schema == s1 {
			userCreateIdx = i
		}
		if step.Type == plan.ChangeCreateTable && step.Table == "orders" && step.Schema == s2 {
			orderCreateIdx = i
		}
	}
	if userCreateIdx == -1 || orderCreateIdx == -1 {
		t.Fatalf("expected ChangeCreateTable for both users and orders, got userIdx=%d, orderIdx=%d", userCreateIdx, orderCreateIdx)
	}
	if userCreateIdx > orderCreateIdx {
		t.Fatalf("expected users (referenced) created before orders (referencing), got userIdx=%d > orderIdx=%d", userCreateIdx, orderCreateIdx)
	}

	// 2. Apply the plan using grizzle.Apply with expected hash verification
	err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{
		ExpectedHash: p.Hash(),
	})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// 3. Plan drop: empty SchemaSQL with AllowDrop: true
	dropPlan, err := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:       grizzle.DialectPostgres,
		TargetSchemas: []string{s1, s2},
		SchemaSQL:     fmt.Sprintf("CREATE TABLE %q._dummy (id INT); CREATE TABLE %q._dummy2 (id INT);", s1, s2),
		AllowDrop:     true,
	})
	if err != nil {
		t.Fatalf("PlanDiff for drop failed: %v", err)
	}

	userDropIdx, orderDropIdx := -1, -1
	for i, step := range dropPlan.Steps {
		if step.Type == plan.ChangeDropTable && step.Table == "users" && step.Schema == s1 {
			userDropIdx = i
		}
		if step.Type == plan.ChangeDropTable && step.Table == "orders" && step.Schema == s2 {
			orderDropIdx = i
		}
	}

	if userDropIdx != -1 && orderDropIdx != -1 {
		if orderDropIdx > userDropIdx {
			t.Fatalf("expected orders (referencing) dropped before users (referenced), got orderDropIdx=%d > userDropIdx=%d", orderDropIdx, userDropIdx)
		}
	}
}

func TestMultiSchema_AdvisoryLockKeyGeneration(t *testing.T) {
	// 1. Single schema compatibility
	singleLockID := grizzle.GenerateLockID("grizzle", "public")
	if singleLockID == 0 {
		t.Errorf("expected non-zero lock ID")
	}

	// 2. Multi-schema combination: sorted schemas
	lock1 := grizzle.GenerateLockID("grizzle", "billing,identity,public")
	lock2 := grizzle.GenerateLockID("grizzle", "billing,identity,public")
	if lock1 != lock2 {
		t.Errorf("expected deterministic lock hash")
	}

	// Distinct schema sets must produce distinct lock IDs
	lockDistinct := grizzle.GenerateLockID("grizzle", "analytics,public")
	if lock1 == lockDistinct {
		t.Errorf("expected distinct lock IDs for distinct schema sets")
	}

	// Permuted schema slice order should yield same lock ID through Options
	opts1 := grizzle.Options{
		TargetSchemas: []string{"public", "identity", "billing"},
		SchemaSQL:     "CREATE TABLE public.t (id INT);",
	}
	opts2 := grizzle.Options{
		TargetSchemas: []string{"billing", "public", "identity"},
		SchemaSQL:     "CREATE TABLE public.t (id INT);",
	}
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	p1, err := grizzle.PlanDiff(context.Background(), db, opts1)
	if err != nil {
		t.Fatalf("PlanDiff opts1 failed: %v", err)
	}
	p2, err := grizzle.PlanDiff(context.Background(), db, opts2)
	if err != nil {
		t.Fatalf("PlanDiff opts2 failed: %v", err)
	}
	if p1.Hash() != p2.Hash() {
		t.Errorf("expected plan hash to be invariant to input TargetSchemas order: %s vs %s", p1.Hash(), p2.Hash())
	}
}
