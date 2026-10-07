package plan_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/plan"
)

func TestPlan_FormatInteractiveSummary_Empty(t *testing.T) {
	p := &plan.Plan{
		TargetSchema: "public",
		Steps:        nil,
	}

	var buf bytes.Buffer
	if err := p.FormatInteractiveSummary(&buf); err != nil {
		t.Fatalf("FormatInteractiveSummary failed: %v", err)
	}

	expected := "Planned changes:\n  No changes. Database schema is already in sync.\n"
	if buf.String() != expected {
		t.Fatalf("expected:\n%q\ngot:\n%q", expected, buf.String())
	}
}

func TestPlan_FormatInteractiveSummary_SpecExample(t *testing.T) {
	p := &plan.Plan{
		TargetSchema: "public",
		Steps: []plan.Step{
			{
				Type:  plan.ChangeCreateTable,
				Table: "accounts",
				SQL:   "CREATE TABLE accounts (id, balance)",
			},
			{
				Type:        plan.ChangeDropColumn,
				Table:       "users",
				Column:      "legacy_role",
				SQL:         `ALTER TABLE "users" DROP COLUMN "legacy_role";`,
				Destructive: true,
			},
		},
		Policy: plan.DropPolicy{
			AllowColumn: true,
		},
	}

	var buf bytes.Buffer
	if err := p.FormatInteractiveSummary(&buf); err != nil {
		t.Fatalf("FormatInteractiveSummary failed: %v", err)
	}

	expected := "Planned changes:\n" +
		"  + CREATE TABLE accounts (id, balance)\n" +
		"  ! DROP COLUMN users.legacy_role [DROP_COLUMN: CRITICAL]\n\n" +
		"[DROP_COLUMN] Column on table \"users\" will be dropped with all existing row values\n"

	if buf.String() != expected {
		t.Fatalf("expected:\n%s\ngot:\n%s", expected, buf.String())
	}
}

func TestPlan_FormatInteractiveSummary_OperationsAndHazards(t *testing.T) {
	p := &plan.Plan{
		TargetSchema: "public",
		Steps: []plan.Step{
			{
				Type:   plan.ChangeAddColumn,
				Table:  "users",
				Column: "email",
				SQL:    `ALTER TABLE "users" ADD COLUMN "email" text;`,
			},
			{
				Type:         plan.ChangeAlterColumn,
				Table:        "users",
				Column:       "bio",
				SQL:          `ALTER TABLE "users" ALTER COLUMN "bio" TYPE varchar(50);`,
				TypeNarrowed: true,
				Destructive:  true,
			},
			{
				Type:  plan.ChangeCreateIndex,
				Table: "users",
				SQL:   `CREATE INDEX "idx_users_email" ON "users" ("email");`,
			},
			{
				Type:        plan.ChangeAttachPartition,
				Table:       "orders_2026",
				ParentTable: "orders",
				SQL:         `ALTER TABLE orders ATTACH PARTITION orders_2026 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');`,
			},
		},
		Policy: plan.DropPolicy{
			AllowColumn: false, // bio alter is destructive and not allowed
		},
	}

	var buf bytes.Buffer
	if err := p.FormatInteractiveSummary(&buf); err != nil {
		t.Fatalf("FormatInteractiveSummary failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "  + ADD COLUMN users.email\n") {
		t.Errorf("missing add column line, got:\n%s", out)
	}
	if !strings.Contains(out, "  ! ALTER COLUMN users.bio [TYPE_NARROW: CRITICAL] [BLOCKED by policy]\n") {
		t.Errorf("missing alter column line with critical badge and blocked badge, got:\n%s", out)
	}
	if !strings.Contains(out, "  + CREATE INDEX idx_users_email ON users [INDEX_BUILD: NOTICE]\n") {
		t.Errorf("missing create index line, got:\n%s", out)
	}
	if !strings.Contains(out, "  ! ATTACH PARTITION orders_2026 TO orders [PARTITION_ATTACH_SCAN: WARNING]\n") {
		t.Errorf("missing attach partition line, got:\n%s", out)
	}
	if !strings.Contains(out, "[TYPE_NARROW] Column on table \"users\" has a destructive type change that may cause data loss or truncation\n") {
		t.Errorf("missing critical hazard description, got:\n%s", out)
	}
}
