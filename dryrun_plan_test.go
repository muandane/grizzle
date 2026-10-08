//go:build integration

package grizzle_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestDryRunVerifyPlan_RejectsTamperedArtifact proves that mutating an
// approval-sensitive field after serialization causes dry-run to fail with
// ErrPlanDrift before verifying a different migration.
func TestDryRunVerifyPlan_RejectsTamperedArtifact(t *testing.T) {
	db := testutil.TestDatabase(t)
	schema := fmt.Sprintf("test_dryrun_plan_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	if err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    `CREATE TABLE widgets (id BIGINT PRIMARY KEY);`,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    `CREATE TABLE widgets (id BIGINT PRIMARY KEY, name TEXT);`,
	})
	if err != nil {
		t.Fatalf("PlanDiff: %v", err)
	}
	data, err := p.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	parsed, hash, err := grizzle.ParsePlanJSON(data)
	if err != nil {
		t.Fatalf("ParsePlanJSON: %v", err)
	}

	// Happy path: approved artifact verifies cleanly.
	if _, err := grizzle.DryRunVerifyPlan(context.Background(), db, parsed, grizzle.ApplyOpts{
		ExpectedHash: hash,
	}); err != nil {
		t.Fatalf("DryRunVerifyPlan on approved plan: %v", err)
	}

	// Tamper policy — hash no longer matches envelope.
	parsed.Policy.AllowTable = true
	_, err = grizzle.DryRunVerifyPlan(context.Background(), db, parsed, grizzle.ApplyOpts{
		ExpectedHash: hash,
	})
	if !errors.Is(err, grizzle.ErrPlanDrift) {
		t.Fatalf("expected ErrPlanDrift for policy tamper, got: %v", err)
	}

	// Tamper target schema in the JSON envelope itself.
	tampered := strings.Replace(string(data), `"target_schema": "`+schema+`"`, `"target_schema": "public"`, 1)
	tp, th, err := grizzle.ParsePlanJSON([]byte(tampered))
	if err != nil {
		t.Fatalf("ParsePlanJSON tampered: %v", err)
	}
	// Envelope hash is the original; recomputed plan hash differs.
	if th == tp.Hash() {
		t.Fatal("tampered target_schema should change plan hash relative to envelope")
	}
	_, err = grizzle.DryRunVerifyPlan(context.Background(), db, tp, grizzle.ApplyOpts{
		ExpectedHash: th,
	})
	if !errors.Is(err, grizzle.ErrPlanDrift) {
		t.Fatalf("expected ErrPlanDrift for target_schema tamper, got: %v", err)
	}
}

// TestDryRunVerifyPlan_HonorsPlanTargetSchema verifies dry-run uses the
// artifact's TargetSchema rather than defaulting to public.
func TestDryRunVerifyPlan_HonorsPlanTargetSchema(t *testing.T) {
	db := testutil.TestDatabase(t)
	schema := fmt.Sprintf("test_dryrun_tgt_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	if err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    `CREATE TABLE items (id BIGINT PRIMARY KEY);`,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    `CREATE TABLE items (id BIGINT PRIMARY KEY, label TEXT);`,
	})
	if err != nil {
		t.Fatalf("PlanDiff: %v", err)
	}
	if p.TargetSchema != schema {
		t.Fatalf("plan TargetSchema=%q want %q", p.TargetSchema, schema)
	}

	res, err := grizzle.DryRunVerifyPlan(context.Background(), db, p, grizzle.ApplyOpts{
		ExpectedHash: p.Hash(),
	})
	if err != nil {
		t.Fatalf("DryRunVerifyPlan: %v", err)
	}
	if res.ExecutedSteps == 0 {
		t.Fatal("expected at least one step executed in dry-run")
	}
}
