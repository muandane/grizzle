package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
)

func TestLocking_StepGrouping(t *testing.T) {
	steps := []plan.Step{
		{SQL: "CREATE TABLE t1 (id int);", NonTx: false},
		{SQL: "CREATE TABLE t2 (id int);", NonTx: false},
		{SQL: "CREATE INDEX CONCURRENTLY idx1 ON t1(id);", NonTx: true},
		{SQL: "CREATE INDEX CONCURRENTLY idx2 ON t2(id);", NonTx: true},
		{SQL: "ALTER TABLE t1 VALIDATE CONSTRAINT fk1;", NonTx: false},
	}

	groups := exec.GroupSteps(steps)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}

	if groups[0].NonTx || len(groups[0].Steps) != 2 {
		t.Errorf("group 0 expected 2 tx steps, got NonTx=%t len=%d", groups[0].NonTx, len(groups[0].Steps))
	}
	if !groups[1].NonTx || len(groups[1].Steps) != 2 {
		t.Errorf("group 1 expected 2 non-tx steps, got NonTx=%t len=%d", groups[1].NonTx, len(groups[1].Steps))
	}
	if groups[2].NonTx || len(groups[2].Steps) != 1 {
		t.Errorf("group 2 expected 1 tx step, got NonTx=%t len=%d", groups[2].NonTx, len(groups[2].Steps))
	}
}

func TestLocking_ForeignKeyNotValidAndValidate(t *testing.T) {
	connStr := os.Getenv("POSTGRES_DSN")
	if connStr == "" {
		connStr = "postgres://postgres:postgres@localhost:5432/grizzle_test?sslmode=disable"
	}
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_fk_nv_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initialSQL := `
		CREATE TABLE orgs (id BIGINT PRIMARY KEY);
		CREATE TABLE users (id BIGINT PRIMARY KEY, org_id BIGINT);
	`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// Add foreign key
	desiredSQL := `
		CREATE TABLE orgs (id BIGINT PRIMARY KEY);
		CREATE TABLE users (
			id BIGINT PRIMARY KEY,
			org_id BIGINT,
			CONSTRAINT fk_users_org FOREIGN KEY (org_id) REFERENCES orgs(id)
		);
	`

	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}

	var hasNotValid, hasValidate bool
	for _, s := range p.Steps {
		if strings.Contains(s.SQL, "NOT VALID") {
			hasNotValid = true
		}
		if s.Type == grizzle.ChangeValidateConstraint {
			hasValidate = true
		}
	}

	if !hasNotValid {
		t.Errorf("expected step with NOT VALID, got steps: %+v", p.Steps)
	}
	if !hasValidate {
		t.Errorf("expected step with ChangeValidateConstraint, got steps: %+v", p.Steps)
	}

	// Apply migration
	err = grizzle.Apply(context.Background(), db, p, grizzle.ApplyOpts{
		ExpectedHash: p.Hash(),
	})
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
}

func TestLocking_RecomputeDiffPostLock(t *testing.T) {
	connStr := os.Getenv("POSTGRES_DSN")
	if connStr == "" {
		connStr = "postgres://postgres:postgres@localhost:5432/grizzle_test?sslmode=disable"
	}
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_recompute_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initialSQL := `CREATE TABLE items (id BIGINT PRIMARY KEY);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	desiredSQL := `
		CREATE TABLE items (id BIGINT PRIMARY KEY, name TEXT);
	`

	// 1. Generate plan before concurrent modification
	preLockPlan, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	preLockHash := preLockPlan.Hash()

	// 2. Interleave a concurrent modification: someone manually adds a column or table
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s.items ADD COLUMN concurrent_col TEXT;`, schema))
	if err != nil {
		t.Fatalf("failed concurrent alter: %v", err)
	}

	// 3. Applying with preLockHash should abort with ErrPlanDrift because post-lock diff differs!
	err = grizzle.Apply(context.Background(), db, preLockPlan, grizzle.ApplyOpts{
		ExpectedHash: preLockHash,
	})
	if err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("expected ErrPlanDrift on stale pre-lock plan, got: %v", err)
	}
}

func TestLocking_NonConcurrentIndexes(t *testing.T) {
	connStr := os.Getenv("POSTGRES_DSN")
	if connStr == "" {
		connStr = "postgres://postgres:postgres@localhost:5432/grizzle_test?sslmode=disable"
	}
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres locking test, database not reachable: %v", err)
	}

	schema := fmt.Sprintf("test_concurrent_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initialSQL := `CREATE TABLE docs (id BIGINT PRIMARY KEY, title TEXT);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	desiredSQL := `
		CREATE TABLE docs (id BIGINT PRIMARY KEY, title TEXT);
		CREATE INDEX idx_docs_title ON docs(title);
	`

	// 1. By default, CONCURRENTLY is emitted
	planDefault, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    desiredSQL,
	})
	if err != nil {
		t.Fatalf("default plan failed: %v", err)
	}
	if len(planDefault.Steps) != 1 || !planDefault.Steps[0].NonTx || !strings.Contains(planDefault.Steps[0].SQL, "CONCURRENTLY") {
		t.Errorf("expected CONCURRENTLY step with NonTx=true, got: %+v", planDefault.Steps)
	}

	// 2. With NonConcurrentIndexes: true, CONCURRENTLY is omitted
	planNonConcurrent, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:              grizzle.DialectPostgres,
		TargetSchema:         schema,
		SchemaSQL:            desiredSQL,
		NonConcurrentIndexes: true,
	})
	if err != nil {
		t.Fatalf("non-concurrent plan failed: %v", err)
	}
	if len(planNonConcurrent.Steps) != 1 || planNonConcurrent.Steps[0].NonTx || strings.Contains(planNonConcurrent.Steps[0].SQL, "CONCURRENTLY") {
		t.Errorf("expected standard index step without CONCURRENTLY and NonTx=false, got: %+v", planNonConcurrent.Steps)
	}
}
