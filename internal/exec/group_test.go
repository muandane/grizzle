package exec_test

import (
	"testing"

	"github.com/yourorg/grizzle/internal/exec"
	"github.com/yourorg/grizzle/internal/plan"
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
	var addFKGroup, validateGroup int = -1, -1
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
