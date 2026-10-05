package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/plan"
)

func TestOptions_FunctionalOptions(t *testing.T) {
	opts := grizzle.Options{}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	backfillFn := func(ctx context.Context, tx *sql.Tx, table, oldCol, newCol string) error {
		return nil
	}
	randFn := func() float64 { return 0.5 }

	applyOptions := []grizzle.Option{
		grizzle.WithLogger(logger),
		grizzle.WithDialect(grizzle.DialectPostgres),
		grizzle.WithTargetSchema("my_schema"),
		grizzle.WithAllowDrop(true),
		grizzle.WithExcludeTables("asynq_*", "temp_*"),
		grizzle.WithIncludeTables("users", "orders"),
		grizzle.WithAcceptHazards(plan.HazardDropColumn, plan.HazardTypeNarrow),
		grizzle.WithNonConcurrentIndexes(true),
		grizzle.WithLockTimeout(10 * time.Second),
		grizzle.WithStatementTimeout(30 * time.Second),
		grizzle.WithMaxRetries(5),
		grizzle.WithRandFloat(randFn),
		grizzle.WithStrictScope(true),
		grizzle.WithRenames(map[string]string{"users.legacy": "modern"}),
		grizzle.WithExpandContract(true),
		grizzle.WithBackfill(backfillFn),
	}

	for _, opt := range applyOptions {
		opt(&opts)
	}

	if opts.Logger != logger {
		t.Errorf("logger not set")
	}
	if opts.Dialect != grizzle.DialectPostgres {
		t.Errorf("dialect not set")
	}
	if opts.TargetSchema != "my_schema" {
		t.Errorf("target schema not set")
	}
	if !opts.AllowDrop {
		t.Errorf("allow drop not set")
	}
	if len(opts.ExcludeTables) != 2 || opts.ExcludeTables[0] != "asynq_*" {
		t.Errorf("exclude tables not set")
	}
	if len(opts.IncludeTables) != 2 || opts.IncludeTables[0] != "users" {
		t.Errorf("include tables not set")
	}
	if len(opts.AcceptHazards) != 2 || opts.AcceptHazards[0] != plan.HazardDropColumn {
		t.Errorf("accept hazards not set")
	}
	if !opts.NonConcurrentIndexes {
		t.Errorf("non-concurrent indexes not set")
	}
	if opts.LockTimeout != 10*time.Second {
		t.Errorf("lock timeout not set")
	}
	if opts.StatementTimeout != 30*time.Second {
		t.Errorf("statement timeout not set")
	}
	if opts.MaxRetries != 5 {
		t.Errorf("max retries not set")
	}
	if opts.RandFloat == nil || opts.RandFloat() != 0.5 {
		t.Errorf("rand float not set")
	}
	if !opts.StrictScope {
		t.Errorf("strict scope not set")
	}
	if opts.Renames["users.legacy"] != "modern" {
		t.Errorf("renames not set")
	}
	if !opts.ExpandContract {
		t.Errorf("expand contract not set")
	}
	if opts.Backfill == nil {
		t.Errorf("backfill not set")
	}
}

func TestOptions_Validate(t *testing.T) {
	// Empty schema
	o1 := grizzle.Options{SchemaSQL: "   "}
	if err := o1.Validate(); !errors.Is(err, grizzle.ErrEmptySchema) {
		t.Errorf("expected ErrEmptySchema, got: %v", err)
	}

	// StrictScope with empty IncludeTables
	o2 := grizzle.Options{
		SchemaSQL:   "CREATE TABLE t (id INT);",
		StrictScope: true,
	}
	if err := o2.Validate(); !errors.Is(err, grizzle.ErrStrictScope) {
		t.Errorf("expected ErrStrictScope, got: %v", err)
	}

	// Unsupported dialect
	o3 := grizzle.Options{
		SchemaSQL: "CREATE TABLE t (id INT);",
		Dialect:   "mysql",
	}
	if err := o3.Validate(); err == nil {
		t.Errorf("expected error for unsupported dialect, got nil")
	}

	// Valid options
	o4 := grizzle.Options{
		SchemaSQL: "CREATE TABLE t (id INT);",
		Dialect:   grizzle.DialectPostgres,
	}
	if err := o4.Validate(); err != nil {
		t.Errorf("expected valid options to pass, got: %v", err)
	}
}

func TestApply_EdgeCases(t *testing.T) {
	ctx := context.Background()

	// 1. Nil plan
	if err := grizzle.Apply(ctx, nil, nil, grizzle.ApplyOpts{}); err == nil {
		t.Errorf("expected error for nil plan, got nil")
	}

	// 2. Nil db
	p := &grizzle.Plan{}
	if err := grizzle.Apply(ctx, nil, p, grizzle.ApplyOpts{}); err == nil {
		t.Errorf("expected error for nil db, got nil")
	}

	// 3. Direct execution fallback (SchemaSQL == "")
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer db.Close()

	directPlan := &grizzle.Plan{
		Steps: []plan.Step{
			{Type: plan.ChangeCreateTable, SQL: "CREATE TABLE fallback (id INT PRIMARY KEY);"},
		},
		Policy: plan.DropPolicy{AllowTable: true},
	}
	if err := grizzle.Apply(ctx, db, directPlan, grizzle.ApplyOpts{}); err != nil {
		t.Fatalf("direct plan execution failed: %v", err)
	}

	// Direct execution with destructive blocked by policy
	blockedPlan := &grizzle.Plan{
		Steps: []plan.Step{
			{Type: plan.ChangeDropTable, Table: "fallback", SQL: "DROP TABLE fallback;", Destructive: true},
		},
		Policy: plan.DropPolicy{AllowTable: false},
	}
	if err := grizzle.Apply(ctx, db, blockedPlan, grizzle.ApplyOpts{}); err == nil {
		t.Errorf("expected destructive violation error, got nil")
	}

	// Direct execution with hazard blocked
	hazardPlan := &grizzle.Plan{
		Steps: []plan.Step{
			{Type: plan.ChangeDropTable, Table: "fallback", SQL: "DROP TABLE fallback;", Destructive: true},
		},
		Policy: plan.DropPolicy{AllowTable: true},
	}
	if err := grizzle.Apply(ctx, db, hazardPlan, grizzle.ApplyOpts{}); !errors.Is(err, grizzle.ErrHazardBlocked) {
		t.Errorf("expected ErrHazardBlocked, got: %v", err)
	}

	// Direct execution with SQL syntax error
	badSQLPlan := &grizzle.Plan{
		Steps: []plan.Step{
			{Type: plan.ChangeCreateTable, SQL: "INVALID SQL SYNTAX HERE;"},
		},
	}
	if err := grizzle.Apply(ctx, db, badSQLPlan, grizzle.ApplyOpts{}); err == nil {
		t.Errorf("expected execution failed error, got nil")
	}
}

