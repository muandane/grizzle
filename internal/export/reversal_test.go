package export_test

import (
	"strings"
	"testing"
	"time"

	"github.com/muandane/grizzle/internal/export"
	"github.com/muandane/grizzle/internal/plan"
)

// TestExport_Reversals_NewStepTypes verifies down-migration SQL for the
// managed-surface step types (policies, RLS flags, functions, triggers,
// views) and that extension creation remains irreversible.
func TestExport_Reversals_NewStepTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		step       plan.Step
		wantRev    string // expected reversal SQL (exact), or substring when prefixed "SUBSTR:"
		irreversib bool   // expected to make the whole migration irreversible
	}{
		{
			name:    "policy create drops policy",
			step:    plan.Step{Type: plan.ChangeCreatePolicy, Table: "docs", SQL: `CREATE POLICY "docs_select" ON "public"."docs" FOR SELECT USING (true);`},
			wantRev: "DROP POLICY IF EXISTS docs_select ON docs;",
		},
		{
			name:    "enable RLS reverses to disable",
			step:    plan.Step{Type: plan.ChangeEnableRLS, Table: "docs", SQL: `ALTER TABLE "public"."docs" ENABLE ROW LEVEL SECURITY;`},
			wantRev: "ALTER TABLE docs DISABLE ROW LEVEL SECURITY;",
		},
		{
			name:    "force RLS reverses to no force",
			step:    plan.Step{Type: plan.ChangeForceRLS, Table: "docs", SQL: `ALTER TABLE "public"."docs" FORCE ROW LEVEL SECURITY;`},
			wantRev: "ALTER TABLE docs NO FORCE ROW LEVEL SECURITY;",
		},
		{
			name:    "function create drops function with args",
			step:    plan.Step{Type: plan.ChangeCreateFunction, Table: "add", SQL: `CREATE OR REPLACE FUNCTION "public"."add"(a integer, b integer) RETURNS integer LANGUAGE sql AS $fn$ SELECT a + b $fn$;`},
			wantRev: `DROP FUNCTION IF EXISTS "public"."add"(a integer, b integer);`,
		},
		{
			name:    "trigger create drops trigger on table",
			step:    plan.Step{Type: plan.ChangeCreateTrigger, Table: "docs", SQL: `CREATE TRIGGER trg_after AFTER INSERT ON "public"."docs" FOR EACH ROW EXECUTE FUNCTION "public"."notify_fn"();`},
			wantRev: "DROP TRIGGER IF EXISTS trg_after ON docs;",
		},
		{
			name:    "view create drops view",
			step:    plan.Step{Type: plan.ChangeCreateView, Table: "docs_recent", SQL: `CREATE VIEW "public"."docs_recent" AS SELECT * FROM "public"."docs";`},
			wantRev: "DROP VIEW IF EXISTS docs_recent;",
		},
		{
			name:    "matview create drops materialized view",
			step:    plan.Step{Type: plan.ChangeCreateView, Table: "docs_summary", SQL: `CREATE MATERIALIZED VIEW "public"."docs_summary" AS SELECT count(*) FROM "public"."docs";`},
			wantRev: "DROP MATERIALIZED VIEW IF EXISTS docs_summary;",
		},
		{
			name:    "matview refresh is a no-op reversal",
			step:    plan.Step{Type: plan.ChangeRefreshMatView, Table: "docs_summary", SQL: `REFRESH MATERIALIZED VIEW "public"."docs_summary";`},
			wantRev: "SUBSTR:no reversal required",
		},
		{
			name:       "extension create is irreversible",
			step:       plan.Step{Type: plan.ChangeCreateExtension, Table: "pgcrypto", SQL: `CREATE EXTENSION IF NOT EXISTS pgcrypto;`},
			irreversib: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &plan.Plan{TargetSchema: "public", Steps: []plan.Step{tt.step}}
			artifacts, err := export.Export(p, export.FormatGoose, "v0.1.0", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("export failed: %v", err)
			}
			content := artifacts[0].Content
			hasDown := strings.Contains(content, "-- +goose Down")
			if hasDown == tt.irreversib {
				t.Fatalf("expected hasDown=%v, content:\n%s", !tt.irreversib, content)
			}
			if tt.irreversib {
				return
			}
			rev := tt.wantRev
			if after, ok := strings.CutPrefix(rev, "SUBSTR:"); ok {
				if !strings.Contains(content, after) {
					t.Fatalf("expected reversal to contain %q, content:\n%s", after, content)
				}
				return
			}
			if !strings.Contains(content, rev+"\n") {
				t.Fatalf("expected reversal %q in down section, content:\n%s", rev, content)
			}
		})
	}
}
