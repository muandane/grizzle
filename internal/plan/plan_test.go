package plan_test

import (
	"bytes"
	"errors"
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

func TestPlan_PublicationNarrowingRequiresDropPolicyAndHazard(t *testing.T) {
	p := &plan.Plan{Steps: []plan.Step{{
		Type:        plan.ChangeAlterPublication,
		Table:       "docs_pub",
		SQL:         `ALTER PUBLICATION "docs_pub" DROP TABLE "public"."archive";`,
		Destructive: true,
	}}}
	if p.Policy.IsAllowed(p.Steps[0]) {
		t.Fatal("destructive ALTER_PUBLICATION must require AllowDropPublication")
	}
	p.Policy.AllowDropPublication = true
	if !p.Policy.IsAllowed(p.Steps[0]) {
		t.Fatal("AllowDropPublication should permit destructive publication ALTER")
	}
	if err := p.ValidateHazards(nil); err == nil {
		t.Fatal("publication narrowing must require DROP_PUBLICATION hazard acceptance")
	}
	if err := p.ValidateHazards([]plan.HazardCode{plan.HazardDropPublication}); err != nil {
		t.Fatalf("accepted DROP_PUBLICATION hazard should pass: %v", err)
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

func TestPlan_GeneratedRewrite_RequiresAcceptHazards(t *testing.T) {
	p := &plan.Plan{
		Steps: []plan.Step{{
			Type:               plan.ChangeAlterColumn,
			Table:              "products",
			SQL:                "ALTER TABLE products DROP COLUMN tax; ALTER TABLE products ADD COLUMN tax numeric GENERATED ALWAYS AS (price * 0.2) STORED;",
			IsGeneratedRewrite: true,
		}},
	}
	err := p.ValidateHazards(nil)
	if err == nil {
		t.Fatal("expected ValidateHazards to block GENERATED_REWRITE without acceptance")
	}
	var he *plan.HazardError
	if !errors.As(err, &he) {
		t.Fatalf("expected HazardError, got %T: %v", err, err)
	}
	found := false
	for _, h := range he.Hazards {
		if h.Code == plan.HazardGeneratedRewrite && h.Level == plan.HazardLevelCritical {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected CRITICAL GENERATED_REWRITE in HazardError, got %+v", he.Hazards)
	}
	if err := p.ValidateHazards([]plan.HazardCode{plan.HazardGeneratedRewrite}); err != nil {
		t.Fatalf("accepted GENERATED_REWRITE should pass: %v", err)
	}
	// Hash stable across hazard analysis (hazards are derived, not hashed).
	h1 := p.Hash()
	_ = p.Hazards()
	if p.Hash() != h1 {
		t.Fatal("Hazards() must not change plan hash")
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

func TestPlan_ValidateHazards(t *testing.T) {
	p := &plan.Plan{
		Steps: []plan.Step{
			{Type: plan.ChangeDropTable, Table: "users", Destructive: true},
			{Type: plan.ChangeDropColumn, Table: "orders", Destructive: true},
		},
	}

	// 1. Blocked when hazards not accepted
	err := p.ValidateHazards(nil)
	if err == nil {
		t.Fatalf("expected ValidateHazards to return error, got nil")
	}
	var herr *plan.HazardError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HazardError, got: %T (%v)", err, err)
	}
	if len(herr.Hazards) != 2 {
		t.Errorf("expected 2 unaccepted hazards, got %d", len(herr.Hazards))
	}

	// 2. Partially accepted still blocks
	err = p.ValidateHazards([]plan.HazardCode{plan.HazardDropTable})
	if err == nil {
		t.Fatalf("expected partial accept to still return error, got nil")
	}

	// 3. Fully accepted passes
	err = p.ValidateHazards([]plan.HazardCode{plan.HazardDropTable, plan.HazardDropColumn})
	if err != nil {
		t.Errorf("expected fully accepted hazards to pass, got: %v", err)
	}
}

func TestPlan_ValidatePolicy(t *testing.T) {
	p := &plan.Plan{
		Steps: []plan.Step{
			{Type: plan.ChangeCreateTable, Table: "users", Destructive: false},
			{Type: plan.ChangeDropTable, Table: "old_users", Destructive: true},
			{Type: plan.ChangeDropColumn, Table: "orders", Destructive: true},
		},
		Policy: plan.DropPolicy{
			AllowTable:  false,
			AllowColumn: true,
		},
	}

	// 1. One violation
	err := p.ValidatePolicy()
	if err == nil {
		t.Fatalf("expected ValidatePolicy to return error, got nil")
	}
	var verr *plan.DestructiveViolationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected *DestructiveViolationError, got: %T (%v)", err, err)
	}
	if len(verr.Violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(verr.Violations))
	}
	if verr.Violations[0].Table != "old_users" {
		t.Errorf("expected violation table old_users, got %s", verr.Violations[0].Table)
	}

	// 2. All disallowed -> multiple violations collected
	p.Policy.AllowColumn = false
	err = p.ValidatePolicy()
	if !errors.As(err, &verr) {
		t.Fatalf("expected *DestructiveViolationError, got: %T (%v)", err, err)
	}
	if len(verr.Violations) != 2 {
		t.Fatalf("expected 2 violations, got %d", len(verr.Violations))
	}

	// 3. All allowed -> nil
	p.Policy.AllowTable = true
	p.Policy.AllowColumn = true
	if err := p.ValidatePolicy(); err != nil {
		t.Errorf("expected ValidatePolicy to pass when policy allows, got: %v", err)
	}
}
