package lint

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

func TestDisallowFloatForFinancialData_L032(t *testing.T) {
	t.Run("flags financial columns with floating-point types", func(t *testing.T) {
		s := &schema.Schema{
			Tables: map[string]*schema.Table{
				"invoices": {
					Name: "invoices",
					Columns: map[string]*schema.Column{
						"id":           {Name: "id", DataType: "bigint"},
						"total_amount": {Name: "total_amount", DataType: "float"},
						"unit_price":   {Name: "unit_price", DataType: "real"},
						"user_balance": {Name: "user_balance", DataType: "double precision"},
						"item_cost":    {Name: "item_cost", DataType: "float4"},
						"service_fee":  {Name: "service_fee", DataType: "float8"},
						"total":        {Name: "total", DataType: "FLOAT(24)"},
					},
				},
			},
		}

		diags := Lint(s, DisallowFloatForFinancialData{})
		if len(diags) != 6 {
			t.Fatalf("expected 6 diagnostics for financial float columns, got %d: %+v", len(diags), diags)
		}

		for _, d := range diags {
			if d.RuleID != "L032" {
				t.Errorf("expected RuleID L032, got %s", d.RuleID)
			}
			if d.Severity != SeverityError {
				t.Errorf("expected SeverityError, got %v", d.Severity)
			}
			if d.Table != "invoices" {
				t.Errorf("expected table invoices, got %s", d.Table)
			}
			if !strings.Contains(d.Message, "for financial data; use NUMERIC or DECIMAL") {
				t.Errorf("unexpected message: %s", d.Message)
			}
		}
	})

	t.Run("passes for numeric types and non-financial float columns", func(t *testing.T) {
		s := &schema.Schema{
			Tables: map[string]*schema.Table{
				"invoices": {
					Name: "invoices",
					Columns: map[string]*schema.Column{
						"id":           {Name: "id", DataType: "bigint"},
						"total_amount": {Name: "total_amount", DataType: "numeric(10, 2)"},
						"unit_price":   {Name: "unit_price", DataType: "decimal(12, 4)"},
						"user_balance": {Name: "user_balance", DataType: "numeric"},
						"temperature":  {Name: "temperature", DataType: "float"},
						"sensor_ratio": {Name: "sensor_ratio", DataType: "double precision"},
					},
				},
			},
		}

		diags := Lint(s, DisallowFloatForFinancialData{})
		if len(diags) != 0 {
			t.Errorf("expected 0 diagnostics, got %d: %+v", len(diags), diags)
		}
	})

	t.Run("handles nil and empty schema gracefully", func(t *testing.T) {
		if diags := Lint(nil, DisallowFloatForFinancialData{}); len(diags) != 0 {
			t.Errorf("expected 0 diagnostics for nil schema, got %d", len(diags))
		}
		empty := &schema.Schema{Tables: map[string]*schema.Table{}}
		if diags := Lint(empty, DisallowFloatForFinancialData{}); len(diags) != 0 {
			t.Errorf("expected 0 diagnostics for empty schema, got %d", len(diags))
		}
	})
}

func TestRequireTableComment_L016(t *testing.T) {
	t.Run("flags tables missing comments in postgres dialect", func(t *testing.T) {
		s := &schema.Schema{
			Dialect: "postgres",
			Tables: map[string]*schema.Table{
				"users": {
					Name:    "users",
					Comment: "",
				},
				"orders": {
					Name:    "orders",
					Comment: "   ",
				},
				"audits": {
					Name:    "audits",
					Comment: "Auditing log table",
				},
			},
		}

		diags := Lint(s, RequireTableComment{})
		if len(diags) != 2 {
			t.Fatalf("expected 2 diagnostics, got %d: %+v", len(diags), diags)
		}

		wantTables := map[string]bool{"orders": true, "users": true}
		for _, d := range diags {
			if d.RuleID != "L016" {
				t.Errorf("expected RuleID L016, got %s", d.RuleID)
			}
			if d.Severity != SeverityInfo {
				t.Errorf("expected SeverityInfo, got %v", d.Severity)
			}
			if !wantTables[d.Table] {
				t.Errorf("unexpected flagged table: %s", d.Table)
			}
			wantMsg := `table "` + d.Table + `" has no COMMENT ON TABLE description`
			if d.Message != wantMsg {
				t.Errorf("expected message %q, got %q", wantMsg, d.Message)
			}
		}
	})

	t.Run("skips sqlite dialect", func(t *testing.T) {
		s := &schema.Schema{
			Dialect: "sqlite",
			Tables: map[string]*schema.Table{
				"users": {
					Name:    "users",
					Comment: "",
				},
			},
		}

		diags := Lint(s, RequireTableComment{})
		if len(diags) != 0 {
			t.Errorf("expected 0 diagnostics for sqlite schema, got %d: %+v", len(diags), diags)
		}
	})

	t.Run("skips table partitions", func(t *testing.T) {
		s := &schema.Schema{
			Dialect: "postgres",
			Tables: map[string]*schema.Table{
				"events_2026_01": {
					Name:        "events_2026_01",
					Comment:     "",
					PartitionOf: &schema.PartitionOf{Parent: "events"},
				},
			},
		}

		diags := Lint(s, RequireTableComment{})
		if len(diags) != 0 {
			t.Errorf("expected 0 diagnostics for partition table, got %d: %+v", len(diags), diags)
		}
	})
}

func TestDisallowCascadeDeleteOnMultiTenant_L033(t *testing.T) {
	t.Run("flags foreign keys with ON DELETE CASCADE on multi-tenant columns", func(t *testing.T) {
		s := &schema.Schema{
			Tables: map[string]*schema.Table{
				"projects": {
					Name: "projects",
					ForeignKeys: map[string]*schema.ForeignKey{
						"fk_projects_tenant": {
							Name:       "fk_projects_tenant",
							TableName:  "projects",
							Definition: "FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE",
						},
						"fk_projects_org": {
							Name:       "fk_projects_org",
							TableName:  "projects",
							Definition: "FOREIGN KEY (org_id) REFERENCES orgs (id) on delete cascade",
						},
						"fk_projects_organization": {
							Name:       "fk_projects_organization",
							TableName:  "projects",
							Definition: "FOREIGN KEY (organization_id) REFERENCES organizations (id) ON DELETE CASCADE",
						},
						"fk_projects_account": {
							Name:       "fk_projects_account",
							TableName:  "projects",
							Definition: "FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE",
						},
					},
				},
			},
		}

		diags := Lint(s, DisallowCascadeDeleteOnMultiTenant{})
		if len(diags) != 4 {
			t.Fatalf("expected 4 diagnostics, got %d: %+v", len(diags), diags)
		}

		for _, d := range diags {
			if d.RuleID != "L033" {
				t.Errorf("expected RuleID L033, got %s", d.RuleID)
			}
			if d.Severity != SeverityWarning {
				t.Errorf("expected SeverityWarning, got %v", d.Severity)
			}
			if !strings.Contains(d.Message, "uses ON DELETE CASCADE on multi-tenant isolation column; prefer ON DELETE RESTRICT") {
				t.Errorf("unexpected message: %s", d.Message)
			}
		}
	})

	t.Run("passes for safe foreign keys", func(t *testing.T) {
		s := &schema.Schema{
			Tables: map[string]*schema.Table{
				"projects": {
					Name: "projects",
					ForeignKeys: map[string]*schema.ForeignKey{
						"fk_projects_tenant_restrict": {
							Name:       "fk_projects_tenant_restrict",
							TableName:  "projects",
							Definition: "FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT",
						},
						"fk_projects_tenant_no_action": {
							Name:       "fk_projects_tenant_no_action",
							TableName:  "projects",
							Definition: "FOREIGN KEY (tenant_id) REFERENCES tenants (id)",
						},
						"fk_items_order_cascade": {
							Name:       "fk_items_order_cascade",
							TableName:  "projects",
							Definition: "FOREIGN KEY (order_id) REFERENCES orders (id) ON DELETE CASCADE",
						},
					},
				},
			},
		}

		diags := Lint(s, DisallowCascadeDeleteOnMultiTenant{})
		if len(diags) != 0 {
			t.Errorf("expected 0 diagnostics, got %d: %+v", len(diags), diags)
		}
	})
}

func TestCompositeIndexMaxColumns_L049(t *testing.T) {
	t.Run("flags composite indexes spanning more than 4 columns", func(t *testing.T) {
		s := &schema.Schema{
			Tables: map[string]*schema.Table{
				"events": {
					Name: "events",
					Indexes: map[string]*schema.Index{
						"idx_events_5_cols": {
							Name:       "idx_events_5_cols",
							Definition: "CREATE INDEX idx_events_5_cols ON events (c1, c2, c3, c4, c5)",
						},
						"idx_events_6_cols": {
							Name:       "idx_events_6_cols",
							Definition: "CREATE INDEX idx_events_6_cols ON events (a, b, c, d, e, f)",
						},
					},
				},
			},
		}

		diags := Lint(s, CompositeIndexMaxColumns{})
		if len(diags) != 2 {
			t.Fatalf("expected 2 diagnostics, got %d: %+v", len(diags), diags)
		}

		for _, d := range diags {
			if d.RuleID != "L049" {
				t.Errorf("expected RuleID L049, got %s", d.RuleID)
			}
			if d.Severity != SeverityWarning {
				t.Errorf("expected SeverityWarning, got %v", d.Severity)
			}
			if !strings.Contains(d.Message, "maximum recommended is 4); risk of index bloat") {
				t.Errorf("unexpected message: %s", d.Message)
			}
		}
	})

	t.Run("passes for indexes with 4 or fewer columns", func(t *testing.T) {
		s := &schema.Schema{
			Tables: map[string]*schema.Table{
				"events": {
					Name: "events",
					Indexes: map[string]*schema.Index{
						"idx_events_1": {
							Name:       "idx_events_1",
							Definition: "CREATE INDEX idx_events_1 ON events (c1)",
						},
						"idx_events_4": {
							Name:       "idx_events_4",
							Definition: "CREATE INDEX idx_events_4 ON events (c1, c2, c3, c4)",
						},
					},
				},
			},
		}

		diags := Lint(s, CompositeIndexMaxColumns{})
		if len(diags) != 0 {
			t.Errorf("expected 0 diagnostics, got %d: %+v", len(diags), diags)
		}
	})
}

func TestDisallowLowCardinalityIndexPrefix_L051(t *testing.T) {
	t.Run("flags composite indexes with low-cardinality leading boolean column", func(t *testing.T) {
		s := &schema.Schema{
			Tables: map[string]*schema.Table{
				"users": {
					Name: "users",
					Columns: map[string]*schema.Column{
						"id":         {Name: "id", DataType: "bigint"},
						"is_active":  {Name: "is_active", DataType: "smallint"},
						"created_at": {Name: "created_at", DataType: "timestamptz"},
						"archived":   {Name: "archived", DataType: "boolean"},
						"status":     {Name: "status", DataType: "text"},
					},
					Indexes: map[string]*schema.Index{
						"idx_users_active_created": {
							Name:       "idx_users_active_created",
							Definition: "CREATE INDEX idx_users_active_created ON users (is_active, created_at)",
						},
						"idx_users_archived_created": {
							Name:       "idx_users_archived_created",
							Definition: "CREATE INDEX idx_users_archived_created ON users (archived, created_at)",
						},
						"idx_users_deleted_status": {
							Name:       "idx_users_deleted_status",
							Definition: "CREATE INDEX idx_users_deleted_status ON users (deleted, status)",
						},
					},
				},
			},
		}

		diags := Lint(s, DisallowLowCardinalityIndexPrefix{})
		if len(diags) != 3 {
			t.Fatalf("expected 3 diagnostics, got %d: %+v", len(diags), diags)
		}

		for _, d := range diags {
			if d.RuleID != "L051" {
				t.Errorf("expected RuleID L051, got %s", d.RuleID)
			}
			if d.Severity != SeverityWarning {
				t.Errorf("expected SeverityWarning, got %v", d.Severity)
			}
			if !strings.Contains(d.Message, "has low-cardinality leading boolean column") {
				t.Errorf("unexpected message: %s", d.Message)
			}
		}
	})

	t.Run("passes for single-column boolean index or composite with non-boolean prefix", func(t *testing.T) {
		s := &schema.Schema{
			Tables: map[string]*schema.Table{
				"users": {
					Name: "users",
					Columns: map[string]*schema.Column{
						"id":         {Name: "id", DataType: "bigint"},
						"is_active":  {Name: "is_active", DataType: "boolean"},
						"created_at": {Name: "created_at", DataType: "timestamptz"},
					},
					Indexes: map[string]*schema.Index{
						"idx_users_single_active": {
							Name:       "idx_users_single_active",
							Definition: "CREATE INDEX idx_users_single_active ON users (is_active)",
						},
						"idx_users_created_active": {
							Name:       "idx_users_created_active",
							Definition: "CREATE INDEX idx_users_created_active ON users (created_at, is_active)",
						},
					},
				},
			},
		}

		diags := Lint(s, DisallowLowCardinalityIndexPrefix{})
		if len(diags) != 0 {
			t.Errorf("expected 0 diagnostics, got %d: %+v", len(diags), diags)
		}
	})
}
