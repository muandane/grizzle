package postgres_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func TestGenerateAlterPolicySQL(t *testing.T) {
	tests := []struct {
		name      string
		schema    string
		table     string
		policy    *schema.Policy
		oldPolicy *schema.Policy
		expected  string
	}{
		{
			name:   "using expression update",
			schema: "public",
			table:  "docs",
			policy: &schema.Policy{
				Name:  "docs_select",
				Using: "owner_id = 2",
			},
			expected: `ALTER POLICY "docs_select" ON "public"."docs" USING (owner_id = 2);`,
		},
		{
			name:   "roles and with check update",
			schema: "tenant_1",
			table:  "invoices",
			policy: &schema.Policy{
				Name:      "invoices_insert",
				Roles:     []string{"editor", "admin"},
				WithCheck: "amount > 0",
			},
			expected: `ALTER POLICY "invoices_insert" ON "tenant_1"."invoices" TO "editor", "admin" WITH CHECK (amount > 0);`,
		},
		{
			name:   "roles, using, and with check update",
			schema: "public",
			table:  "items",
			policy: &schema.Policy{
				Name:      "items_rw",
				Roles:     []string{"app_user"},
				Using:     "tenant_id = 1",
				WithCheck: "tenant_id = 1 AND active = true",
			},
			expected: `ALTER POLICY "items_rw" ON "public"."items" TO "app_user" USING (tenant_id = 1) WITH CHECK (tenant_id = 1 AND active = true);`,
		},
		{
			name:   "roles with public pseudo-role unquoted",
			schema: "public",
			table:  "docs",
			policy: &schema.Policy{
				Name:  "docs_select",
				Roles: []string{"public"},
				Using: "true",
			},
			expected: `ALTER POLICY "docs_select" ON "public"."docs" TO public USING (true);`,
		},
		{
			name:   "reset roles from explicit to default via oldPolicy",
			schema: "public",
			table:  "docs",
			policy: &schema.Policy{
				Name:  "docs_select",
				Using: "true",
			},
			oldPolicy: &schema.Policy{
				Name:  "docs_select",
				Roles: []string{"app_user"},
			},
			expected: `ALTER POLICY "docs_select" ON "public"."docs" TO public USING (true);`,
		},
		{
			name:     "nil policy returns empty",
			schema:   "public",
			table:    "docs",
			policy:   nil,
			expected: "",
		},
		{
			name:     "empty policy name returns empty",
			schema:   "public",
			table:    "docs",
			policy:   &schema.Policy{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if tt.oldPolicy != nil {
				got = postgres.GenerateAlterPolicySQL(tt.schema, tt.table, tt.policy, tt.oldPolicy)
			} else {
				got = postgres.GenerateAlterPolicySQL(tt.schema, tt.table, tt.policy)
			}
			if got != tt.expected {
				t.Errorf("GenerateAlterPolicySQL() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestRenderChange_RenameEnumValue(t *testing.T) {
	c := diff.Change{
		Type:         plan.ChangeRenameEnumValue,
		Schema:       "public",
		Table:        "status",
		OldEnumValue: "pending",
		EnumValue:    "in_progress",
	}

	step, err := postgres.RenderChange("public", c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedSQL := `ALTER TYPE "public"."status" RENAME VALUE 'pending' TO 'in_progress';`
	if step.SQL != expectedSQL {
		t.Errorf("step.SQL = %q, want %q", step.SQL, expectedSQL)
	}
	if step.OldEnumValue != "pending" || step.EnumValue != "in_progress" {
		t.Errorf("step OldEnumValue/EnumValue mismatch: old=%q new=%q", step.OldEnumValue, step.EnumValue)
	}
}

func TestGenerateSequenceSQL(t *testing.T) {
	t.Run("CreateSequence_Default", func(t *testing.T) {
		seq := &schema.Sequence{
			Name: "order_seq",
		}
		sql := postgres.GenerateCreateSequenceSQL("public", seq)
		expected := `CREATE SEQUENCE "public"."order_seq";`
		if sql != expected {
			t.Errorf("got %q, want %q", sql, expected)
		}
	})

	t.Run("CreateSequence_FullParameters", func(t *testing.T) {
		var minVal int64 = 10
		var maxVal int64 = 1000
		seq := &schema.Sequence{
			Name:      "custom_seq",
			DataType:  "integer",
			Start:     50,
			Increment: 5,
			MinValue:  &minVal,
			MaxValue:  &maxVal,
			Cache:     20,
			Cycle:     true,
		}
		sql := postgres.GenerateCreateSequenceSQL("app", seq)
		expected := `CREATE SEQUENCE "app"."custom_seq" AS integer INCREMENT BY 5 MINVALUE 10 MAXVALUE 1000 START WITH 50 CACHE 20 CYCLE;`
		if sql != expected {
			t.Errorf("got %q, want %q", sql, expected)
		}
	})

	t.Run("AlterSequence_Params", func(t *testing.T) {
		oldSeq := &schema.Sequence{
			Name:      "param_seq",
			Increment: 1,
			Cache:     1,
			Cycle:     false,
		}
		newSeq := &schema.Sequence{
			Name:      "param_seq",
			Increment: 10,
			Cache:     5,
			Cycle:     true,
		}
		sql := postgres.GenerateAlterSequenceSQL("public", "param_seq", oldSeq, newSeq)
		expected := `ALTER SEQUENCE "public"."param_seq" INCREMENT BY 10 CACHE 5 CYCLE;`
		if sql != expected {
			t.Errorf("got %q, want %q", sql, expected)
		}
	})

	t.Run("AlterSequence_Ownership_SchemaQualified", func(t *testing.T) {
		newSeq := &schema.Sequence{
			Name:          "invoice_seq",
			OwnedByTable:  "invoices",
			OwnedByColumn: "id",
		}
		sql := postgres.GenerateAlterSequenceSQL("public", "invoice_seq", nil, newSeq)
		expected := `ALTER SEQUENCE "public"."invoice_seq" OWNED BY "public"."invoices"."id";`
		if sql != expected {
			t.Errorf("got %q, want %q", sql, expected)
		}
	})

	t.Run("AlterSequence_Ownership_CrossSchema", func(t *testing.T) {
		newSeq := &schema.Sequence{
			Name:          "cross_seq",
			OwnedByTable:  "billing.invoices",
			OwnedByColumn: "invoice_no",
		}
		sql := postgres.GenerateAlterSequenceSQL("public", "cross_seq", nil, newSeq)
		expected := `ALTER SEQUENCE "public"."cross_seq" OWNED BY "billing"."invoices"."invoice_no";`
		if sql != expected {
			t.Errorf("got %q, want %q", sql, expected)
		}
	})

	t.Run("AlterSequence_Ownership_Detach", func(t *testing.T) {
		oldSeq := &schema.Sequence{
			Name:          "free_seq",
			OwnedByTable:  "legacy_table",
			OwnedByColumn: "col",
		}
		newSeq := &schema.Sequence{
			Name:         "free_seq",
			OwnedByTable: "NONE",
		}
		sql := postgres.GenerateAlterSequenceSQL("public", "free_seq", oldSeq, newSeq)
		expected := `ALTER SEQUENCE "public"."free_seq" OWNED BY NONE;`
		if sql != expected {
			t.Errorf("got %q, want %q", sql, expected)
		}
	})

	t.Run("DropSequence_IfExists", func(t *testing.T) {
		sql := postgres.GenerateDropSequenceSQL("public", "stale_seq")
		expected := `DROP SEQUENCE IF EXISTS "public"."stale_seq";`
		if sql != expected {
			t.Errorf("got %q, want %q", sql, expected)
		}
	})
}

func TestRenderChange_Sequences(t *testing.T) {
	createChange := diff.Change{
		Type:   plan.ChangeCreateSequence,
		Schema: "public",
		Table:  "orders_seq",
		Sequence: &schema.Sequence{
			Name: "orders_seq",
		},
	}
	createStep, err := postgres.RenderChange("public", createChange)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if createStep.SQL != `CREATE SEQUENCE "public"."orders_seq";` {
		t.Errorf("unexpected create SQL: %q", createStep.SQL)
	}
	if createStep.Sequence == nil || createStep.Sequence.Name != "orders_seq" {
		t.Errorf("expected Sequence metadata on Step")
	}

	dropChange := diff.Change{
		Type:        plan.ChangeDropSequence,
		Schema:      "public",
		Table:       "old_seq",
		Destructive: true,
	}
	dropStep, err := postgres.RenderChange("public", dropChange)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dropStep.SQL != `DROP SEQUENCE IF EXISTS "public"."old_seq";` {
		t.Errorf("unexpected drop SQL: %q", dropStep.SQL)
	}
}

func TestGeneratePostgresStep_ValidateConstraintMissingPayload(t *testing.T) {
	c := diff.Change{
		Type:  plan.ChangeValidateConstraint,
		Table: "orders",
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("expected error return, but GeneratePostgresStep panicked: %v", r)
		}
	}()

	_, err := postgres.GeneratePostgresStep("public", c, postgres.RenderOpts{})
	if err == nil {
		t.Fatal("expected error for VALIDATE_CONSTRAINT with missing constraint payload, got nil")
	}
	expectedMsg := `grizzle: VALIDATE_CONSTRAINT change on table "orders" has no constraint payload`
	if err.Error() != expectedMsg {
		t.Errorf("expected error %q, got %q", expectedMsg, err.Error())
	}
}
