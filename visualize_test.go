package grizzle

import (
	"bytes"
	"strings"
	"testing"
)

func TestPlan_Summary(t *testing.T) {
	plan := &Plan{
		TargetSchema: "public",
		Policy: DropPolicy{
			AllowTable:  false,
			AllowColumn: false,
			AllowIndex:  true,
		},
		Steps: []Step{
			{Type: ChangeCreateTable, Table: "users", SQL: "CREATE TABLE users ();"},
			{Type: ChangeAddColumn, Table: "users", SQL: "ALTER TABLE users ADD COLUMN age int;"},
			{Type: ChangeAlterColumn, Table: "users", SQL: "ALTER TABLE users ALTER COLUMN age TYPE bigint;"},
			{Type: ChangeDropIndex, Table: "users", SQL: "DROP INDEX idx_old;", Destructive: true},
			{Type: ChangeDropColumn, Table: "users", SQL: "ALTER TABLE users DROP COLUMN temp;", Destructive: true},
		},
	}

	adds, alters, drops, blocked := plan.Summary()

	if adds != 2 {
		t.Errorf("expected 2 additions, got %d", adds)
	}
	if alters != 1 {
		t.Errorf("expected 1 alteration, got %d", alters)
	}
	if drops != 2 {
		t.Errorf("expected 2 drops, got %d", drops)
	}
	// DropIndex is allowed (AllowIndex: true), DropColumn is blocked (AllowColumn: false)
	if blocked != 1 {
		t.Errorf("expected 1 blocked step, got %d", blocked)
	}
}

func TestPlan_Format_PlainText(t *testing.T) {
	plan := &Plan{
		TargetSchema: "public",
		Policy:       DropPolicy{AllowColumn: false},
		Steps: []Step{
			{Type: ChangeCreateTable, Table: "orgs", SQL: "CREATE TABLE orgs ();"},
			{Type: ChangeDropColumn, Table: "users", SQL: "ALTER TABLE users DROP COLUMN old;", Destructive: true},
		},
	}

	out := plan.String()

	if !strings.Contains(out, `Grizzle Migration Plan for schema "public":`) {
		t.Errorf("missing header in plan string: %s", out)
	}
	if !strings.Contains(out, "+ [CREATE_TABLE]   CREATE TABLE orgs ();") {
		t.Errorf("missing addition line: %s", out)
	}
	if !strings.Contains(out, "- [DROP_COLUMN]    ALTER TABLE users DROP COLUMN old; [BLOCKED by policy]") {
		t.Errorf("missing blocked drop line: %s", out)
	}
	if !strings.Contains(out, "Plan: 1 to add, 0 to alter, 1 to destroy (1 blocked by safety policy).") {
		t.Errorf("missing summary line: %s", out)
	}
}

func TestPlan_Format_Color(t *testing.T) {
	plan := &Plan{
		TargetSchema: "public",
		Policy:       DropPolicy{AllowColumn: true},
		Steps: []Step{
			{Type: ChangeCreateTable, Table: "orgs", SQL: "CREATE TABLE orgs ();"},
		},
	}

	var buf bytes.Buffer
	err := plan.Format(&buf, true)
	if err != nil {
		t.Fatalf("Format failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, colorGreen) {
		t.Errorf("expected ANSI green escape in color output, got: %q", out)
	}
}

func TestPlan_Empty(t *testing.T) {
	plan := &Plan{
		TargetSchema: "public",
	}

	out := plan.String()
	if !strings.Contains(out, "No changes. Database schema is already in sync.") {
		t.Errorf("expected no changes message, got: %s", out)
	}
}
