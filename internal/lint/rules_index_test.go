package lint

import (
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

func TestDisallowDuplicateIndex(t *testing.T) {
	s := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Indexes: map[string]*schema.Index{
					"idx_users_email_1": {
						Name:       "idx_users_email_1",
						Definition: "CREATE INDEX idx_users_email_1 ON users (email)",
					},
					"idx_users_email_2": {
						Name:       "idx_users_email_2",
						Definition: "CREATE INDEX idx_users_email_2 ON users (email)",
					},
					"idx_users_name": {
						Name:       "idx_users_name",
						Definition: "CREATE INDEX idx_users_name ON users (name)",
					},
				},
			},
		},
	}

	diags := Lint(s, DisallowDuplicateIndex{})
	if len(diags) != 1 {
		t.Fatalf("expected 1 duplicate index diagnostic, got %d: %+v", len(diags), diags)
	}

	if diags[0].Table != "users" || diags[0].Column != "idx_users_email_2" || diags[0].RuleID != "L047" {
		t.Errorf("unexpected diagnostic: %+v", diags[0])
	}
}

func TestDisallowRedundantPrefixIndex(t *testing.T) {
	s := &schema.Schema{
		Tables: map[string]*schema.Table{
			"orders": {
				Name: "orders",
				Indexes: map[string]*schema.Index{
					"idx_customer": {
						Name:       "idx_customer",
						Definition: "CREATE INDEX idx_customer ON orders (customer_id)",
						IsUnique:   false,
					},
					"idx_customer_date": {
						Name:       "idx_customer_date",
						Definition: "CREATE INDEX idx_customer_date ON orders (customer_id, order_date)",
						IsUnique:   false,
					},
					"uk_customer": {
						Name:       "uk_customer",
						Definition: "CREATE UNIQUE INDEX uk_customer ON orders (customer_id)",
						IsUnique:   true, // unique: not redundant!
					},
				},
			},
		},
	}

	diags := Lint(s, DisallowRedundantPrefixIndex{})
	if len(diags) != 1 {
		t.Fatalf("expected 1 redundant prefix diagnostic (idx_customer), got %d: %+v", len(diags), diags)
	}

	if diags[0].Table != "orders" || diags[0].Column != "idx_customer" || diags[0].RuleID != "L048" {
		t.Errorf("unexpected diagnostic: %+v", diags[0])
	}
}
