package plan_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/plan"
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
	if hazards[0].Level != plan.HazardLevelCritical || hazards[0].Code != plan.HazardDropTable {
		t.Errorf("expected CRITICAL DROP_TABLE, got %s / %s", hazards[0].Level, hazards[0].Code)
	}
	if hazards[1].Level != plan.HazardLevelCritical || hazards[1].Code != plan.HazardNotNullNoDefault {
		t.Errorf("expected CRITICAL NOT_NULL_NO_DEFAULT, got %s / %s", hazards[1].Level, hazards[1].Code)
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

func TestPlan_HasDestructive(t *testing.T) {
	p1 := &plan.Plan{
		Steps: []plan.Step{
			{Type: plan.ChangeCreateTable, Destructive: false},
		},
	}
	if p1.HasDestructive() {
		t.Errorf("expected HasDestructive to be false")
	}

	p2 := &plan.Plan{
		Steps: []plan.Step{
			{Type: plan.ChangeDropTable, Destructive: true},
		},
	}
	if !p2.HasDestructive() {
		t.Errorf("expected HasDestructive to be true")
	}
}

func TestPlan_ErrorTypes(t *testing.T) {
	// HazardError
	hErr := &plan.HazardError{
		Hazards: []plan.Hazard{
			{Code: plan.HazardDropTable},
			{Code: plan.HazardTypeNarrow},
		},
	}
	if !strings.Contains(hErr.Error(), "DROP_TABLE") {
		t.Errorf("unexpected HazardError message: %s", hErr.Error())
	}
	if !hErr.Is(plan.ErrHazardBlocked) {
		t.Errorf("expected HazardError to match ErrHazardBlocked")
	}
	if hErr.Unwrap() != plan.ErrHazardBlocked {
		t.Errorf("expected Unwrap to return ErrHazardBlocked")
	}

	// DestructiveViolationError
	dErrSingle := &plan.DestructiveViolationError{
		Violations: []plan.Step{
			{Type: plan.ChangeDropColumn, Table: "users"},
		},
	}
	if !strings.Contains(dErrSingle.Error(), "DROP_COLUMN on users") {
		t.Errorf("unexpected dErrSingle message: %s", dErrSingle.Error())
	}
	if !dErrSingle.Is(plan.ErrDestructiveBlocked) {
		t.Errorf("expected DestructiveViolationError to match ErrDestructiveBlocked")
	}

	dErrMulti := &plan.DestructiveViolationError{
		Violations: []plan.Step{
			{Type: plan.ChangeDropColumn, Table: "users"},
			{Type: plan.ChangeDropTable, Table: "orders"},
		},
	}
	if !strings.Contains(dErrMulti.Error(), "2 destructive changes") {
		t.Errorf("unexpected dErrMulti message: %s", dErrMulti.Error())
	}

	// DriftError
	driftEmpty := &plan.DriftError{}
	if driftEmpty.Error() != "grizzle: database schema drift detected" {
		t.Errorf("unexpected empty DriftError message: %s", driftEmpty.Error())
	}
	driftWithPlan := &plan.DriftError{
		Plan: &plan.Plan{Steps: []plan.Step{{Type: plan.ChangeCreateTable}}},
	}
	if !strings.Contains(driftWithPlan.Error(), "1 change(s)") {
		t.Errorf("unexpected driftWithPlan message: %s", driftWithPlan.Error())
	}
	if !driftWithPlan.Is(plan.ErrDrift) {
		t.Errorf("expected DriftError to match ErrDrift")
	}
	if driftWithPlan.Unwrap() != plan.ErrDrift {
		t.Errorf("expected Unwrap to return ErrDrift")
	}
}

