package grizzle_test

import (
	"testing"

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
