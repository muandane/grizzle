package exec

import (
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

func TestUnmapShadowExprs(t *testing.T) {
	s := &schema.Schema{Tables: map[string]*schema.Table{
		"docs": {Name: "docs", Columns: map[string]*schema.Column{
			"fingerprint": {
				Name: "fingerprint",
				Generated: &schema.GeneratedColumn{
					Expr:   `encode(_grizzle_shadow_910d3b1d.digest(title, 'sha256'::text), 'hex'::text)`,
					Stored: true,
				},
			},
			"token": {
				Name:         "token",
				DefaultValue: `_grizzle_shadow_910d3b1d.gen_random_uuid()`,
			},
			"plain": {Name: "plain", DefaultValue: `now()`},
		}},
	}}

	unmapShadowExprs(s, []string{"_grizzle_shadow_910d3b1d"})

	if got := s.Tables["docs"].Columns["fingerprint"].Generated.Expr; got != `encode(digest(title, 'sha256'::text), 'hex'::text)` {
		t.Fatalf("generated expr not unmapped: %q", got)
	}
	if got := s.Tables["docs"].Columns["token"].DefaultValue; got != `gen_random_uuid()` {
		t.Fatalf("default not unmapped: %q", got)
	}
	if got := s.Tables["docs"].Columns["plain"].DefaultValue; got != `now()` {
		t.Fatalf("plain default mutated: %q", got)
	}
}
