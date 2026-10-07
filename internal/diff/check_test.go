package diff_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func checkTable(name string, checks map[string]*schema.CheckConstraint) *schema.Table {
	return &schema.Table{
		Name:        name,
		Columns:     map[string]*schema.Column{"id": {Name: "id", DataType: "integer", IsNullable: false}},
		Indexes:     make(map[string]*schema.Index),
		ForeignKeys: make(map[string]*schema.ForeignKey),
		Checks:      checks,
	}
}

func namedCheck(table, name, def string, valid bool) *schema.CheckConstraint {
	return &schema.CheckConstraint{Name: name, TableName: table, Definition: def, IsValid: valid}
}

func emptySchema() *schema.Schema {
	return &schema.Schema{Tables: make(map[string]*schema.Table), Enums: make(map[string]*schema.Enum)}
}

func findCheckChange(t *testing.T, changes []diff.Change, typ plan.ChangeType, name string) *diff.Change {
	t.Helper()
	for i := range changes {
		c := changes[i]
		if c.Type == typ && c.Check != nil && c.Check.Name == name {
			return &changes[i]
		}
	}
	return nil
}

func TestDiff_Check_Add(t *testing.T) {
	live := emptySchema()
	desired := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": checkTable("products", map[string]*schema.CheckConstraint{
				"check_positive_price": namedCheck("products", "check_positive_price", "CHECK (price_cents > 0)", true),
			}),
		},
		Enums: make(map[string]*schema.Enum),
	}

	// New table: CREATE TABLE followed by ADD_CHECK (checks are staged, not embedded).
	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	if len(changes) < 2 || changes[0].Type != plan.ChangeCreateTable {
		t.Fatalf("expected ChangeCreateTable followed by ADD_CHECK, got: %+v", changes)
	}
	add := findCheckChange(t, changes, plan.ChangeAddCheck, "check_positive_price")
	if add == nil {
		t.Fatalf("expected ChangeAddCheck for check_positive_price, got: %+v", changes)
	}
	if add.Destructive {
		t.Errorf("ADD_CHECK must not be destructive")
	}

	// Existing table without the check: ADD_CHECK only.
	liveWithTable := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": checkTable("products", nil),
		},
		Enums: make(map[string]*schema.Enum),
	}
	changes, err = diff.Diff(liveWithTable, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	if got := len(changes); got != 1 {
		t.Fatalf("expected exactly 1 change, got %d: %+v", got, changes)
	}
	if changes[0].Type != plan.ChangeAddCheck || changes[0].Check.Name != "check_positive_price" {
		t.Fatalf("expected ChangeAddCheck, got: %+v", changes[0])
	}
}

func TestDiff_Check_Redefine(t *testing.T) {
	live := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": checkTable("products", map[string]*schema.CheckConstraint{
				"check_positive_price": namedCheck("products", "check_positive_price", "CHECK (price_cents > 0)", true),
			}),
		},
		Enums: make(map[string]*schema.Enum),
	}
	desired := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": checkTable("products", map[string]*schema.CheckConstraint{
				"check_positive_price": namedCheck("products", "check_positive_price", "CHECK (price_cents >= 100)", true),
			}),
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	drop := findCheckChange(t, changes, plan.ChangeDropCheck, "check_positive_price")
	add := findCheckChange(t, changes, plan.ChangeAddCheck, "check_positive_price")
	if drop == nil || add == nil {
		t.Fatalf("expected DROP_CHECK + ADD_CHECK pair for redefinition, got: %+v", changes)
	}
	if !drop.Destructive {
		t.Errorf("DROP_CHECK must be destructive")
	}
}

func TestDiff_Check_ValidateIfInvalid(t *testing.T) {
	live := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": checkTable("products", map[string]*schema.CheckConstraint{
				"check_positive_price": namedCheck("products", "check_positive_price", "CHECK (price_cents > 0) NOT VALID", false),
			}),
		},
		Enums: make(map[string]*schema.Enum),
	}
	desired := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": checkTable("products", map[string]*schema.CheckConstraint{
				"check_positive_price": namedCheck("products", "check_positive_price", "CHECK (price_cents > 0)", true),
			}),
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected exactly 1 change, got %d: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Type != plan.ChangeValidateConstraint {
		t.Fatalf("expected ChangeValidateConstraint, got: %+v", c)
	}
	if c.Check.Name != "check_positive_price" {
		t.Errorf("expected validated check name check_positive_price, got %q", c.Check.Name)
	}
	// The NOT VALID suffix must be stripped from the validated definition.
	if c.Check.Definition == "CHECK (price_cents > 0) NOT VALID" {
		t.Errorf("expected NOT VALID suffix stripped from validated definition")
	}
}

func TestDiff_Check_DropOrphan(t *testing.T) {
	liveProducts := checkTable("products", map[string]*schema.CheckConstraint{
		"check_legacy": namedCheck("products", "check_legacy", "CHECK (price_cents > 0)", true),
		// Auto-named inline check: never auto-dropped when orphaned.
		"products_price_cents_check": namedCheck("products", "products_price_cents_check", "CHECK (price_cents > 0)", true),
	})
	liveProducts.Columns["price_cents"] = &schema.Column{Name: "price_cents", DataType: "integer", IsNullable: false}
	live := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": liveProducts,
		},
		Enums: make(map[string]*schema.Enum),
	}
	desired := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": checkTable("products", nil),
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	if findCheckChange(t, changes, plan.ChangeDropCheck, "check_legacy") == nil {
		t.Fatalf("expected DROP_CHECK for explicitly named orphan check, got: %+v", changes)
	}
	if findCheckChange(t, changes, plan.ChangeDropCheck, "products_price_cents_check") != nil {
		t.Errorf("auto-named orphan check must not be dropped (system artifact protection), got: %+v", changes)
	}
}

func TestDiff_Check_NoDiffWhenInSync(t *testing.T) {
	// Both definitions mirror pg_get_constraintdef canonical output; the
	// trailing NOT VALID form must normalize to the validated base form.
	live := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": checkTable("products", map[string]*schema.CheckConstraint{
				"check_positive_price": namedCheck("products", "check_positive_price", "CHECK ((price_cents > 0)) NOT VALID", false),
			}),
		},
		Enums: make(map[string]*schema.Enum),
	}
	desired := &schema.Schema{
		Tables: map[string]*schema.Table{
			"products": checkTable("products", map[string]*schema.CheckConstraint{
				"check_positive_price": namedCheck("products", "check_positive_price", "CHECK ((price_cents > 0))", true),
			}),
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	if len(changes) != 1 || changes[0].Type != plan.ChangeValidateConstraint {
		t.Fatalf("expected single VALIDATE_CONSTRAINT for NOT VALID live check, got: %+v", changes)
	}
}

func TestDiff_Check_PartitionInheritsNotDiffed(t *testing.T) {
	// A partition child that inherits a CHECK from its parent (conislocal=false)
	// never appears in the IR, so the child must not diff against it.
	liveChild := checkTable("logs_2026", nil)
	liveChild.PartitionOf = &schema.PartitionOf{Parent: "logs", Bounds: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"}
	live := &schema.Schema{
		Tables: map[string]*schema.Table{"logs_2026": liveChild},
		Enums:  make(map[string]*schema.Enum),
	}
	desiredChild := checkTable("logs_2026", nil)
	desiredChild.PartitionOf = &schema.PartitionOf{Parent: "logs", Bounds: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"}
	desired := &schema.Schema{
		Tables: map[string]*schema.Table{"logs_2026": desiredChild},
		Enums:  make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff failed: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no check changes for partition child without local checks, got: %+v", changes)
	}
}

func TestIsAutoGeneratedCheckName(t *testing.T) {
	cols := map[string]*schema.Column{
		"id":          {Name: "id"},
		"price_cents": {Name: "price_cents"},
	}
	cases := []struct {
		table, name string
		want        bool
	}{
		{"products", "products_check", true},
		{"products", "products_check1", true},
		{"products", "products_price_cents_check", true},
		{"products", "products_price_cents_check2", true},
		{"products", "check_positive_price", false},
		{"products", "products_price_check", false}, // no such column "price"
		{"products", "products_positive_price_check", false},
		{"products", "", false},
		{"", "products_check", false},
	}
	for _, tc := range cases {
		if got := schema.IsAutoGeneratedCheckName(tc.table, tc.name, cols); got != tc.want {
			t.Errorf("IsAutoGeneratedCheckName(%q, %q) = %v, want %v", tc.table, tc.name, got, tc.want)
		}
	}
}
