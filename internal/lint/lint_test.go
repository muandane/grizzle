package lint

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

func testSchema() *schema.Schema {
	users := &schema.Table{
		Name:        "users",
		Columns:     map[string]*schema.Column{},
		Indexes:     map[string]*schema.Index{},
		ForeignKeys: map[string]*schema.ForeignKey{},
		PrimaryKey:  &schema.PrimaryKey{Name: "users_pkey", Columns: []string{"id"}},
	}
	users.Columns["id"] = &schema.Column{Name: "id", DataType: "integer", IsIdentity: true}
	users.Columns["email"] = &schema.Column{Name: "email", DataType: "text"}

	posts := &schema.Table{
		Name:        "Posts",
		Columns:     map[string]*schema.Column{},
		Indexes:     map[string]*schema.Index{},
		ForeignKeys: map[string]*schema.ForeignKey{},
		PrimaryKey:  &schema.PrimaryKey{Name: "posts_pkey", Columns: []string{"id"}},
	}
	posts.Columns["id"] = &schema.Column{Name: "id", DataType: "integer", DefaultValue: "nextval('posts_id_seq'::regclass)"}
	posts.Columns["UserId"] = &schema.Column{Name: "UserId", DataType: "integer"}
	posts.ForeignKeys["posts_user_fk"] = &schema.ForeignKey{
		Name:       "posts_user_fk",
		TableName:  "Posts",
		RefTable:   "users",
		Definition: "FOREIGN KEY (UserId) REFERENCES users(id)",
	}

	auditLog := &schema.Table{
		Name:        "audit_log",
		Columns:     map[string]*schema.Column{},
		Indexes:     map[string]*schema.Index{},
		ForeignKeys: map[string]*schema.ForeignKey{},
	}
	auditLog.Columns["id"] = &schema.Column{Name: "id", DataType: "integer"}

	orderItems := &schema.Table{
		Name:        "order_items",
		Columns:     map[string]*schema.Column{},
		Indexes:     map[string]*schema.Index{},
		ForeignKeys: map[string]*schema.ForeignKey{},
		PrimaryKey:  &schema.PrimaryKey{Name: "order_items_pkey", Columns: []string{"id"}},
	}
	orderItems.Columns["id"] = &schema.Column{Name: "id", DataType: "integer", IsIdentity: true}
	orderItems.Columns["order_id"] = &schema.Column{Name: "order_id", DataType: "integer"}
	orderItems.Columns["product_id"] = &schema.Column{Name: "product_id", DataType: "integer"}
	orderItems.ForeignKeys["order_items_order_fk"] = &schema.ForeignKey{
		Name:       "order_items_order_fk",
		TableName:  "order_items",
		RefTable:   "orders",
		Definition: "FOREIGN KEY (order_id, product_id) REFERENCES orders (id, product_id)",
	}
	orderItems.Indexes["order_items_order_idx"] = &schema.Index{
		Name:       "order_items_order_idx",
		TableName:  "order_items",
		Definition: "CREATE INDEX order_items_order_idx ON public.order_items USING btree (product_id, order_id)",
		IsValid:    true,
	}

	return &schema.Schema{
		Name:   "public",
		Tables: map[string]*schema.Table{"users": users, "Posts": posts, "audit_log": auditLog, "order_items": orderItems},
		Enums:  map[string]*schema.Enum{"UserRole": {Name: "UserRole", Values: []string{"admin"}}},
	}
}

func TestMissingPrimaryKey(t *testing.T) {
	diags := Lint(testSchema(), MissingPrimaryKey{})
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(diags), diags)
	}
	d := diags[0]
	if d.RuleID != "L001" || d.Severity != SeverityError || d.Table != "audit_log" {
		t.Errorf("unexpected diagnostic: %+v", d)
	}
}

func TestMissingPrimaryKey_SkipsPartitions(t *testing.T) {
	s := testSchema()
	p := &schema.Table{
		Name:        "orders_2026",
		Columns:     map[string]*schema.Column{},
		Indexes:     map[string]*schema.Index{},
		ForeignKeys: map[string]*schema.ForeignKey{},
		PartitionOf: &schema.PartitionOf{Parent: "orders"},
	}
	s.Tables["orders_2026"] = p
	diags := Lint(s, MissingPrimaryKey{})
	if len(diags) != 1 || diags[0].Table != "audit_log" {
		t.Errorf("partition should be skipped, got: %+v", diags)
	}
}

func TestUnindexedForeignKey(t *testing.T) {
	diags := Lint(testSchema(), UnindexedForeignKey{})
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic (posts.UserId), got %d: %+v", len(diags), diags)
	}
	d := diags[0]
	if d.RuleID != "L002" || d.Table != "Posts" || d.Column != "UserId" {
		t.Errorf("unexpected diagnostic: %+v", d)
	}
}

func TestUnindexedForeignKey_CoveredByPK(t *testing.T) {
	s := &schema.Schema{Tables: map[string]*schema.Table{
		"orders": {
			Name:        "orders",
			Columns:     map[string]*schema.Column{"customer_id": {Name: "customer_id", DataType: "integer"}},
			Indexes:     map[string]*schema.Index{},
			ForeignKeys: map[string]*schema.ForeignKey{},
			PrimaryKey:  &schema.PrimaryKey{Name: "orders_pkey", Columns: []string{"customer_id", "seq"}},
		},
	}}
	s.Tables["orders"].ForeignKeys["orders_customer_fk"] = &schema.ForeignKey{
		Name:       "orders_customer_fk",
		Definition: "FOREIGN KEY (customer_id) REFERENCES customers (id)",
	}
	if diags := Lint(s, UnindexedForeignKey{}); len(diags) != 0 {
		t.Errorf("FK covered by PK should not warn: %+v", diags)
	}
}

func TestNamingConvention(t *testing.T) {
	diags := Lint(testSchema(), NamingConvention{})
	var got []string
	for _, d := range diags {
		got = append(got, d.Table+"."+d.Column)
	}
	want := []string{"Posts.Posts", "Posts.UserId", "UserRole.UserRole"}
	if len(got) != len(want) {
		t.Fatalf("expected diagnostics %v, got %v", want, got)
	}
	for i, g := range got {
		if g != want[i] {
			t.Errorf("diagnostic[%d] = %q, want %q", i, g, want[i])
		}
	}
}

func TestPreferIdentityOverSerial(t *testing.T) {
	diags := Lint(testSchema(), PreferIdentityOverSerial{})
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic (Posts.id nextval), got %d: %+v", len(diags), diags)
	}
	d := diags[0]
	if d.RuleID != "L004" || d.Table != "Posts" || d.Column != "id" {
		t.Errorf("unexpected diagnostic: %+v", d)
	}
}

func TestLint_DeterministicOrder(t *testing.T) {
	a := Lint(testSchema(), DefaultRules()...)
	b := Lint(testSchema(), DefaultRules()...)
	if len(a) != len(b) {
		t.Fatalf("length mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("diagnostics not deterministic at %d:\n%+v\n%+v", i, a[i], b[i])
		}
	}
	for i := 1; i < len(a); i++ {
		if a[i-1].RuleID > a[i].RuleID || (a[i-1].RuleID == a[i].RuleID && a[i-1].Table > a[i].Table) {
			t.Errorf("diagnostics not sorted: %+v before %+v", a[i-1], a[i])
		}
	}
}

func TestHasErrors(t *testing.T) {
	diags := Lint(testSchema(), DefaultRules()...)
	if !HasErrors(diags) {
		t.Error("expected HasErrors=true due to L001")
	}
	if HasErrors(Lint(testSchema(), NamingConvention{})) {
		t.Error("warnings must not count as errors")
	}
}

var updateGolden = flag.Bool("update", false, "update golden files")

func TestGolden_Formats(t *testing.T) {
	diags := Lint(testSchema(), DefaultRules()...)

	var textBuf, jsonBuf, ghBuf bytes.Buffer
	if err := FormatText(&textBuf, diags); err != nil {
		t.Fatalf("FormatText: %v", err)
	}
	if err := FormatJSON(&jsonBuf, diags); err != nil {
		t.Fatalf("FormatJSON: %v", err)
	}
	if err := FormatGitHub(&ghBuf, diags); err != nil {
		t.Fatalf("FormatGitHub: %v", err)
	}

	cases := map[string]string{
		"golden.text": textBuf.String(),
		"golden.json": jsonBuf.String(),
		"golden.gh":   ghBuf.String(),
	}
	goldenDir := filepath.Join("testdata")
	if err := os.MkdirAll(goldenDir, 0750); err != nil {
		t.Fatalf("creating testdata dir: %v", err)
	}
	for name, got := range cases {
		got = strings.ReplaceAll(got, "\r\n", "\n")
		goldenPath := filepath.Join("testdata", name)
		if *updateGolden {
			if err := os.WriteFile(goldenPath, []byte(got), 0600); err != nil { //nolint:gosec // G304: test writes static testdata golden file
				t.Fatalf("failed updating golden file: %v", err)
			}
			t.Logf("Updated golden file %s", goldenPath)
			continue
		}
		want, err := os.ReadFile(goldenPath) //nolint:gosec // G304: test reads static testdata golden file
		if err != nil {
			if os.IsNotExist(err) {
				if err := os.WriteFile(goldenPath, []byte(got), 0600); err != nil { //nolint:gosec // G304: test writes static testdata golden file
					t.Fatalf("failed writing initial golden file: %v", err)
				}
				t.Logf("Created golden file %s", goldenPath)
				continue
			}
			t.Fatalf("failed reading golden file: %v", err)
		}
		if got != string(want) {
			t.Errorf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", name, got, string(want))
		}
	}
}
