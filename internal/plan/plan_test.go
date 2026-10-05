package plan_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yourorg/grizzle/internal/plan"
)

func TestPlan_Summary(t *testing.T) {
	p := &plan.Plan{
		TargetSchema: "public",
		Policy: plan.DropPolicy{
			AllowTable:  false,
			AllowColumn: false,
			AllowIndex:  true,
		},
		Steps: []plan.Step{
			{Type: plan.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users ();"},
			{Type: plan.ChangeAddColumn, Table: "users", SQL: "ALTER TABLE users ADD COLUMN age int;"},
			{Type: plan.ChangeAlterColumn, Table: "users", SQL: "ALTER TABLE users ALTER COLUMN age TYPE bigint;"},
			{Type: plan.ChangeDropIndex, Table: "users", SQL: "DROP INDEX idx_old;", Destructive: true},
			{Type: plan.ChangeDropColumn, Table: "users", SQL: "ALTER TABLE users DROP COLUMN temp;", Destructive: true},
		},
	}

	adds, alters, drops, blocked := p.Summary()
	if adds != 2 {
		t.Errorf("expected 2 additions, got %d", adds)
	}
	if alters != 1 {
		t.Errorf("expected 1 alteration, got %d", alters)
	}
	if drops != 2 {
		t.Errorf("expected 2 drops, got %d", drops)
	}
	if blocked != 1 {
		t.Errorf("expected 1 blocked, got %d", blocked)
	}
}

func TestPlan_Hazards(t *testing.T) {
	p := &plan.Plan{
		TargetSchema: "public",
		Steps: []plan.Step{
			{Type: plan.ChangeDropTable, Table: "legacy", SQL: "DROP TABLE legacy;", Destructive: true},
			{Type: plan.ChangeAddColumn, Table: "users", SQL: "ALTER TABLE users ADD COLUMN name text NOT NULL;", ColumnNotNull: true, ColumnHasDefault: false},
		},
	}

	hazards := p.Hazards()
	if len(hazards) != 2 {
		t.Fatalf("expected 2 hazards, got %d", len(hazards))
	}
	if hazards[0].Level != plan.HazardLevelCritical {
		t.Errorf("expected CRITICAL for DROP TABLE, got %s", hazards[0].Level)
	}
	if hazards[1].Level != plan.HazardLevelWarning {
		t.Errorf("expected WARNING for NOT NULL ADD COLUMN, got %s", hazards[1].Level)
	}
}

func TestPlan_Format(t *testing.T) {
	p := &plan.Plan{
		TargetSchema: "public",
		Steps: []plan.Step{
			{Type: plan.ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users ();"},
		},
	}

	var buf bytes.Buffer
	if err := p.Format(&buf, false); err != nil {
		t.Fatalf("Format failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CREATE TABLE users ();") {
		t.Errorf("Format missing step SQL: %s", out)
	}
}
