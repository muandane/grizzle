package exec_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
)

func TestGroupSteps_FKValidateInSeparateGroup(t *testing.T) {
	steps := []plan.Step{
		{Type: plan.ChangeCreateTable, SQL: "CREATE TABLE parents (id int);", NonTx: false},
		{Type: plan.ChangeAddFK, SQL: "ALTER TABLE children ADD CONSTRAINT fk_parent FOREIGN KEY (parent_id) REFERENCES parents(id) NOT VALID;", NonTx: false},
		{Type: plan.ChangeValidateConstraint, SQL: "ALTER TABLE children VALIDATE CONSTRAINT fk_parent;", NonTx: false},
	}

	groups := exec.GroupSteps(steps)

	if len(groups) < 2 {
		t.Fatalf("expected at least 2 groups (ADD FK in tx 1, VALIDATE in tx 2), got %d groups: %+v", len(groups), groups)
	}

	// Verify ADD FK is in a group before VALIDATE CONSTRAINT
	addFKGroup, validateGroup := -1, -1
	for i, g := range groups {
		for _, s := range g.Steps {
			if s.Type == plan.ChangeAddFK {
				addFKGroup = i
			}
			if s.Type == plan.ChangeValidateConstraint {
				validateGroup = i
			}
		}
	}

	if addFKGroup == -1 || validateGroup == -1 {
		t.Fatalf("expected both addFK and validate steps in groups, got addFK=%d, validate=%d", addFKGroup, validateGroup)
	}

	if addFKGroup == validateGroup {
		t.Fatalf("VALIDATE CONSTRAINT must NOT be in the same transaction group as ADD FK (both in group %d)", addFKGroup)
	}

	if validateGroup <= addFKGroup {
		t.Fatalf("VALIDATE CONSTRAINT group (%d) must execute after ADD FK group (%d)", validateGroup, addFKGroup)
	}
}

func TestGroupSteps_TableDriven(t *testing.T) {
	tests := []struct {
		name          string
		steps         []plan.Step
		expectedLens  []int
		expectedNonTx []bool
	}{
		{
			name:          "empty steps",
			steps:         nil,
			expectedLens:  nil,
			expectedNonTx: nil,
		},
		{
			name: "single tx step",
			steps: []plan.Step{
				{Type: plan.ChangeCreateTable, SQL: "CREATE TABLE t1 (id int);", NonTx: false},
			},
			expectedLens:  []int{1},
			expectedNonTx: []bool{false},
		},
		{
			name: "single non-tx step",
			steps: []plan.Step{
				{Type: plan.ChangeCreateIndex, SQL: "CREATE INDEX CONCURRENTLY idx1 ON t1(id);", NonTx: true},
			},
			expectedLens:  []int{1},
			expectedNonTx: []bool{true},
		},
		{
			name: "consecutive tx steps",
			steps: []plan.Step{
				{Type: plan.ChangeCreateTable, SQL: "CREATE TABLE t1 (id int);", NonTx: false},
				{Type: plan.ChangeCreateTable, SQL: "CREATE TABLE t2 (id int);", NonTx: false},
				{Type: plan.ChangeAddColumn, SQL: "ALTER TABLE t1 ADD COLUMN c1 text;", NonTx: false},
			},
			expectedLens:  []int{3},
			expectedNonTx: []bool{false},
		},
		{
			name: "consecutive non-tx steps",
			steps: []plan.Step{
				{Type: plan.ChangeCreateIndex, SQL: "CREATE INDEX CONCURRENTLY idx1 ON t1(id);", NonTx: true},
				{Type: plan.ChangeCreateIndex, SQL: "CREATE INDEX CONCURRENTLY idx2 ON t2(id);", NonTx: true},
			},
			expectedLens:  []int{2},
			expectedNonTx: []bool{true},
		},
		{
			name: "alternating tx and non-tx steps",
			steps: []plan.Step{
				{Type: plan.ChangeCreateTable, SQL: "CREATE TABLE t1 (id int);", NonTx: false},
				{Type: plan.ChangeCreateIndex, SQL: "CREATE INDEX CONCURRENTLY idx1 ON t1(id);", NonTx: true},
				{Type: plan.ChangeCreateTable, SQL: "CREATE TABLE t2 (id int);", NonTx: false},
				{Type: plan.ChangeCreateIndex, SQL: "CREATE INDEX CONCURRENTLY idx2 ON t2(id);", NonTx: true},
			},
			expectedLens:  []int{1, 1, 1, 1},
			expectedNonTx: []bool{false, true, false, true},
		},
		{
			name: "tx followed by multiple validate constraints",
			steps: []plan.Step{
				{Type: plan.ChangeCreateTable, SQL: "CREATE TABLE t1 (id int);", NonTx: false},
				{Type: plan.ChangeValidateConstraint, SQL: "ALTER TABLE t1 VALIDATE CONSTRAINT c1;", NonTx: false},
				{Type: plan.ChangeValidateConstraint, SQL: "ALTER TABLE t1 VALIDATE CONSTRAINT c2;", NonTx: false},
			},
			expectedLens:  []int{1, 1, 1},
			expectedNonTx: []bool{false, false, false},
		},
		{
			name: "alter enum non-tx step executed outside transaction",
			steps: []plan.Step{
				{Type: plan.ChangeCreateEnum, SQL: "CREATE TYPE status AS ENUM ('active');", NonTx: false},
				{Type: plan.ChangeAlterEnum, SQL: "ALTER TYPE status ADD VALUE 'inactive';", NonTx: true},
				{Type: plan.ChangeCreateTable, SQL: "CREATE TABLE users (id int, s status);", NonTx: false},
			},
			expectedLens:  []int{1, 1, 1},
			expectedNonTx: []bool{false, true, false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			groups := exec.GroupSteps(tc.steps)
			if len(groups) != len(tc.expectedLens) {
				t.Fatalf("expected %d groups, got %d: %+v", len(tc.expectedLens), len(groups), groups)
			}
			for i, g := range groups {
				if len(g.Steps) != tc.expectedLens[i] {
					t.Errorf("group %d: expected %d steps, got %d", i, tc.expectedLens[i], len(g.Steps))
				}
				if g.NonTx != tc.expectedNonTx[i] {
					t.Errorf("group %d: expected NonTx=%t, got %t", i, tc.expectedNonTx[i], g.NonTx)
				}
			}
		})
	}
}
