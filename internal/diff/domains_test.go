package diff_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func domain(name, baseType string, nullable bool, def string, checks ...*schema.CheckConstraint) *schema.Domain {
	return &schema.Domain{
		Name:       name,
		BaseType:   baseType,
		IsNullable: nullable,
		Default:    def,
		Checks:     checks,
	}
}

func check(name, def string) *schema.CheckConstraint {
	return &schema.CheckConstraint{Name: name, TableName: "", Definition: def, IsValid: true}
}

func TestDiff_Domains_CreateMissing(t *testing.T) {
	live := &schema.Schema{Name: "public"}
	desired := &schema.Schema{Name: "public", Domains: map[string]*schema.Domain{
		"positive_int": domain("positive_int", "integer", true, ""),
	}}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeCreateDomain] != 1 || len(changes) != 1 {
		t.Fatalf("expected single CREATE_DOMAIN, got %v", types)
	}
	if changes[0].Destructive {
		t.Fatalf("create must not be destructive: %+v", changes[0])
	}
}

func TestDiff_Domains_LiveOnlyDropGated(t *testing.T) {
	live := &schema.Schema{Name: "public", Domains: map[string]*schema.Domain{
		"positive_int": domain("positive_int", "integer", true, ""),
	}}
	desired := &schema.Schema{Name: "public"}

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropDomain] != 1 || len(changes) != 1 {
		t.Fatalf("expected single DROP_DOMAIN, got %v", types)
	}
	if !changes[0].Destructive {
		t.Fatalf("drop must be destructive: %+v", changes[0])
	}
}

func TestDiff_Domains_CheckAddAltersDomain(t *testing.T) {
	mk := func(checks []*schema.CheckConstraint) *schema.Schema {
		return &schema.Schema{Name: "public", Domains: map[string]*schema.Domain{
			"positive_int": domain("positive_int", "integer", true, "", checks...),
		}}
	}
	live := mk(nil)
	desired := mk([]*schema.CheckConstraint{check("positive_int_check", "CHECK ((VALUE > 0))")})

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeAlterDomain] != 1 || len(changes) != 1 {
		t.Fatalf("expected single ALTER_DOMAIN (ADD CONSTRAINT), got %v", types)
	}
	for _, c := range changes {
		if c.Destructive {
			t.Fatalf("constraint add must not be destructive: %+v", c)
		}
	}
}

func TestDiff_Domains_CheckRemoveIsDestructive(t *testing.T) {
	mk := func(checks []*schema.CheckConstraint) *schema.Schema {
		return &schema.Schema{Name: "public", Domains: map[string]*schema.Domain{
			"positive_int": domain("positive_int", "integer", true, "", checks...),
		}}
	}
	live := mk([]*schema.CheckConstraint{check("positive_int_check", "CHECK ((VALUE > 0))")})
	desired := mk(nil)

	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropDomainConstraint] != 1 || len(changes) != 1 {
		t.Fatalf("expected single DROP_DOMAIN_CONSTRAINT, got %v", types)
	}
	if !changes[0].Destructive {
		t.Fatalf("constraint drop must be destructive: %+v", changes[0])
	}
}

func TestDiff_Domains_CheckRedefinitionDropsThenAdds(t *testing.T) {
	mk := func(def string) *schema.Schema {
		return &schema.Schema{Name: "public", Domains: map[string]*schema.Domain{
			"positive_int": domain("positive_int", "integer", true, "", check("positive_int_check", def)),
		}}
	}

	changes, err := diff.Diff(mk("CHECK ((VALUE > 0))"), mk("CHECK ((VALUE >= 0))"), "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropDomainConstraint] != 1 || types[plan.ChangeAlterDomain] != 1 || len(changes) != 2 {
		t.Fatalf("expected DROP_DOMAIN_CONSTRAINT then ALTER_DOMAIN, got %v", types)
	}
	if changes[0].Type != plan.ChangeDropDomainConstraint || !changes[0].Destructive {
		t.Fatalf("first change must be destructive DROP_DOMAIN_CONSTRAINT, got %+v", changes[0])
	}
	if changes[1].Type != plan.ChangeAlterDomain || changes[1].Destructive {
		t.Fatalf("second change must be non-destructive ALTER_DOMAIN, got %+v", changes[1])
	}
}

func TestDiff_Domains_BaseTypeDriftRebuilds(t *testing.T) {
	mk := func(base string) *schema.Schema {
		return &schema.Schema{Name: "public", Domains: map[string]*schema.Domain{
			"amount": domain("amount", base, true, ""),
		}}
	}

	changes, err := diff.Diff(mk("integer"), mk("bigint"), "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	types := countTypes(changes)
	if types[plan.ChangeDropDomainRetype] != 1 || types[plan.ChangeCreateDomain] != 1 || len(changes) != 2 {
		t.Fatalf("expected DROP_DOMAIN_RETYPE + CREATE_DOMAIN for base type drift, got %v", types)
	}
	if !changes[0].Destructive || changes[0].Type != plan.ChangeDropDomainRetype {
		t.Fatalf("first change must be destructive DROP_DOMAIN_RETYPE, got %+v", changes[0])
	}
	if changes[1].Type != plan.ChangeCreateDomain {
		t.Fatalf("second change must be CREATE_DOMAIN, got %+v", changes[1])
	}
}

func TestDiff_Domains_NoOpWhenIdentical(t *testing.T) {
	mk := func() *schema.Schema {
		return &schema.Schema{Name: "public", Domains: map[string]*schema.Domain{
			"positive_int": domain("positive_int", "integer", true, "0", check("positive_int_check", "CHECK ((VALUE > 0))")),
		}}
	}

	changes, err := diff.Diff(mk(), mk(), "public", "", scope.Filters{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("identical domains must produce no changes, got %+v", changes)
	}
}
