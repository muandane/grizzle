package history_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muandane/grizzle/internal/history"
	"github.com/muandane/grizzle/internal/plan"
)

func TestHistory_SQLiteReadWrite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// 1. EnsureTable creates table
	if err := history.EnsureTable(ctx, db, "sqlite", ""); err != nil {
		t.Fatalf("EnsureTable failed: %v", err)
	}

	// Initial GetLatest should return nil
	latest, err := history.GetLatest(ctx, db, "sqlite", "")
	if err != nil {
		t.Fatalf("GetLatest failed: %v", err)
	}
	if latest != nil {
		t.Errorf("expected nil latest record, got %+v", latest)
	}

	// 2. Insert record via RecordPlan
	p := &plan.Plan{
		TargetSchema: "main",
		Steps: []plan.Step{
			{Type: plan.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users (id int);"},
		},
	}

	start := time.Now()
	err = history.RecordPlan(ctx, db, "sqlite", "", p, 42*time.Millisecond)
	if err != nil {
		t.Fatalf("RecordPlan failed: %v", err)
	}

	// 3. GetLatest should return the inserted record
	latest, err = history.GetLatest(ctx, db, "sqlite", "")
	if err != nil {
		t.Fatalf("GetLatest failed: %v", err)
	}
	if latest == nil {
		t.Fatalf("expected non-nil latest record")
	}

	if latest.PlanHash != p.Hash() {
		t.Errorf("expected plan hash %s, got %s", p.Hash(), latest.PlanHash)
	}
	if latest.DurationMs != 42 {
		t.Errorf("expected duration 42ms, got %d", latest.DurationMs)
	}
	if latest.AppliedBy == "" {
		t.Errorf("expected non-empty applied_by")
	}
	if latest.StepsJSON == "" {
		t.Errorf("expected non-empty steps_json")
	}

	// 4. List records
	records, err := history.List(ctx, db, "sqlite", "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("expected 1 record in list, got %d", len(records))
	}
	if records[0].ID != latest.ID {
		t.Errorf("expected list record ID %d, got %d", latest.ID, records[0].ID)
	}
	if time.Since(start) < 0 {
		t.Errorf("unexpected time flow")
	}
}

func TestHistory_LifecycleAndResolution(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// 1. Initial empty checks
	applied, partial, failed, err := history.Summary(ctx, db, "sqlite", "")
	if err != nil {
		t.Fatalf("Summary on empty table failed: %v", err)
	}
	if applied != 0 || partial != 0 || failed != 0 {
		t.Errorf("expected 0 summary counts, got %d, %d, %d", applied, partial, failed)
	}

	res, err := history.GetByID(ctx, db, "sqlite", "", 1)
	if err != nil {
		t.Fatalf("GetByID on missing table failed: %v", err)
	}
	if res != nil {
		t.Errorf("expected nil result from GetByID, got %+v", res)
	}

	_, err = history.ResolveLatest(ctx, db, "sqlite", "", "should fail")
	if err != history.ErrNoFailedMigration {
		t.Fatalf("expected ErrNoFailedMigration, got %v", err)
	}

	// 2. RecordBaseline
	baseRec, err := history.RecordBaseline(ctx, db, "sqlite", "", "hash_base", "initial baseline")
	if err != nil {
		t.Fatalf("RecordBaseline failed: %v", err)
	}
	if baseRec.Status != "baseline" {
		t.Errorf("expected status 'baseline', got %s", baseRec.Status)
	}
	if baseRec.PlanHash != "hash_base" {
		t.Errorf("expected plan hash 'hash_base', got %s", baseRec.PlanHash)
	}

	// Summary after baseline
	applied, partial, failed, err = history.Summary(ctx, db, "sqlite", "")
	if err != nil {
		t.Fatalf("Summary failed: %v", err)
	}
	if applied != 1 || partial != 0 || failed != 0 {
		t.Errorf("expected 1 applied (baseline), got %d applied, %d partial, %d failed", applied, partial, failed)
	}

	// 3. Record a failed migration
	err = history.Insert(ctx, db, "sqlite", "", history.Record{
		PlanHash:   "hash_fail",
		Status:     "failed",
		FailedStep: 2,
		Error:      "syntax error near foo",
		AppliedAt:  time.Now().UTC(),
		DurationMs: 15,
		AppliedBy:  "testuser",
		StepsJSON:  "[]",
	})
	if err != nil {
		t.Fatalf("Insert failed migration: %v", err)
	}

	applied, partial, failed, err = history.Summary(ctx, db, "sqlite", "")
	if err != nil {
		t.Fatalf("Summary failed: %v", err)
	}
	if applied != 1 || partial != 0 || failed != 1 {
		t.Errorf("expected 1 applied, 0 partial, 1 failed, got %d, %d, %d", applied, partial, failed)
	}

	// 4. ResolveLatest
	resolved, err := history.ResolveLatest(ctx, db, "sqlite", "", "fixed table manually")
	if err != nil {
		t.Fatalf("ResolveLatest failed: %v", err)
	}
	if resolved.Status != "resolved" {
		t.Errorf("expected status 'resolved', got %s", resolved.Status)
	}
	if resolved.PlanHash != "hash_fail" {
		t.Errorf("expected resolved plan hash 'hash_fail', got %s", resolved.PlanHash)
	}

	// Now resolving again should return ErrNoFailedMigration because no failed/partial exist
	_, err = history.ResolveLatest(ctx, db, "sqlite", "", "another resolve")
	if err != history.ErrNoFailedMigration {
		t.Fatalf("expected ErrNoFailedMigration after resolve, got %v", err)
	}

	// Summary after resolve
	applied, partial, failed, err = history.Summary(ctx, db, "sqlite", "")
	if err != nil {
		t.Fatalf("Summary failed: %v", err)
	}
	if applied != 2 || partial != 0 || failed != 0 {
		t.Errorf("expected 2 applied (1 baseline + 1 resolved), 0 failed, got %d applied, %d partial, %d failed", applied, partial, failed)
	}

	// 5. Test ListEntries with limit & order
	err = history.Insert(ctx, db, "sqlite", "", history.Record{
		PlanHash:   "hash_3",
		Status:     "applied",
		AppliedAt:  time.Now().UTC(),
		DurationMs: 25,
		AppliedBy:  "testuser",
		StepsJSON:  "[]",
	})
	if err != nil {
		t.Fatalf("Insert 3rd migration: %v", err)
	}

	entriesDesc, err := history.ListEntries(ctx, db, "sqlite", "", 2, true)
	if err != nil {
		t.Fatalf("ListEntries desc failed: %v", err)
	}
	if len(entriesDesc) != 2 {
		t.Fatalf("expected 2 entries in limited query, got %d", len(entriesDesc))
	}
	if entriesDesc[0].PlanHash != "hash_3" {
		t.Errorf("expected first entry in desc order to be hash_3, got %s", entriesDesc[0].PlanHash)
	}

	entriesAsc, err := history.ListEntries(ctx, db, "sqlite", "", 0, false)
	if err != nil {
		t.Fatalf("ListEntries asc failed: %v", err)
	}
	if len(entriesAsc) != 3 {
		t.Fatalf("expected 3 entries in total, got %d", len(entriesAsc))
	}
	if entriesAsc[0].PlanHash != "hash_base" {
		t.Errorf("expected first entry in asc order to be hash_base, got %s", entriesAsc[0].PlanHash)
	}

	// 6. GetByID on existing ID
	recByID, err := history.GetByID(ctx, db, "sqlite", "", entriesAsc[1].ID)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if recByID == nil || recByID.ID != entriesAsc[1].ID {
		t.Errorf("expected recByID matching ID %d, got %+v", entriesAsc[1].ID, recByID)
	}
}
