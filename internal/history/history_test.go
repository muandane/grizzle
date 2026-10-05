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
	defer db.Close()

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
