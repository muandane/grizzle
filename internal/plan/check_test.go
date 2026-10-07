package plan_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/plan"
)

func TestSortSteps_CheckConstraints(t *testing.T) {
	steps := []plan.Step{
		{Type: plan.ChangeValidateConstraint, Table: "products", SQL: `ALTER TABLE "products" VALIDATE CONSTRAINT "check_price";`, ValidatesCheck: true},
		{Type: plan.ChangeAddCheck, Table: "products", SQL: `ALTER TABLE "products" ADD CONSTRAINT "check_price" CHECK (price_cents > 0) NOT VALID;`},
		{Type: plan.ChangeCreateTable, Table: "products", SQL: `CREATE TABLE "products" (id integer);`},
		{Type: plan.ChangeDropCheck, Table: "products", SQL: `ALTER TABLE "products" DROP CONSTRAINT IF EXISTS "check_legacy";`, Destructive: true},
	}

	plan.SortSteps(steps)

	want := []plan.ChangeType{
		plan.ChangeDropCheck,
		plan.ChangeCreateTable,
		plan.ChangeAddCheck,
		plan.ChangeValidateConstraint,
	}
	for i, exp := range want {
		if steps[i].Type != exp {
			t.Errorf("step %d: expected %s, got %s", i, exp, steps[i].Type)
		}
	}
}

func TestPlan_Hazards_CheckConstraints(t *testing.T) {
	p := &plan.Plan{
		TargetSchema: "public",
		Steps: []plan.Step{
			{
				Type:        plan.ChangeDropCheck,
				Table:       "products",
				SQL:         `ALTER TABLE "products" DROP CONSTRAINT IF EXISTS "check_legacy";`,
				Destructive: true,
			},
			{
				Type:           plan.ChangeValidateConstraint,
				Table:          "products",
				SQL:            `ALTER TABLE "products" VALIDATE CONSTRAINT "check_price";`,
				ValidatesCheck: true,
			},
			{
				// FK validate steps must not emit the CHECK scan hazard.
				Type:  plan.ChangeValidateConstraint,
				Table: "children",
				SQL:   `ALTER TABLE "children" VALIDATE CONSTRAINT "fk_parent";`,
			},
		},
	}

	hazards := p.Hazards()
	var dropCheck, checkScan bool
	for _, h := range hazards {
		switch h.Code {
		case plan.HazardDropCheck:
			dropCheck = true
			if h.Level != plan.HazardLevelNotice {
				t.Errorf("DROP_CHECK hazard must be NOTICE, got %s", h.Level)
			}
		case plan.HazardCheckValidateScan:
			checkScan = true
			if h.Table != "products" {
				t.Errorf("CHECK_VALIDATE_SCAN hazard must attach to the check validate step (products), got table %q", h.Table)
			}
		}
	}
	if !dropCheck {
		t.Errorf("expected DROP_CHECK hazard")
	}
	if !checkScan {
		t.Errorf("expected CHECK_VALIDATE_SCAN hazard for ValidatesCheck step")
	}
}

func TestPlan_DropPolicy_AllowCheck(t *testing.T) {
	dropCheckStep := plan.Step{
		Type:        plan.ChangeDropCheck,
		Table:       "products",
		SQL:         `ALTER TABLE "products" DROP CONSTRAINT IF EXISTS "check_legacy";`,
		Destructive: true,
	}

	policy := plan.DropPolicy{}
	if policy.IsAllowed(dropCheckStep) {
		t.Errorf("DROP_CHECK must be blocked by default-deny policy")
	}

	policy.AllowCheck = true
	if !policy.IsAllowed(dropCheckStep) {
		t.Errorf("DROP_CHECK must be allowed when AllowCheck is set")
	}

	// AllowFK alone must not unlock DROP_CHECK.
	policy = plan.DropPolicy{AllowFK: true}
	if policy.IsAllowed(dropCheckStep) {
		t.Errorf("AllowFK must not unlock DROP_CHECK")
	}
}

func TestPlan_Summary_CheckConstraints(t *testing.T) {
	p := &plan.Plan{
		TargetSchema: "public",
		Steps: []plan.Step{
			{Type: plan.ChangeAddCheck, Table: "products", SQL: "ADD"},
			{Type: plan.ChangeDropCheck, Table: "products", SQL: "DROP", Destructive: true},
		},
	}
	if got := p.Additions(); got != 1 {
		t.Errorf("Additions() = %d, want 1 (ADD_CHECK)", got)
	}
	if got := p.Deletions(); got != 1 {
		t.Errorf("Deletions() = %d, want 1 (DROP_CHECK)", got)
	}
}
