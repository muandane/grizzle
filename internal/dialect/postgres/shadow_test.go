package postgres_test

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
)

func TestRewriteShadowSQL(t *testing.T) {
	shadowMap := map[string]string{
		"identity": "_grizzle_shadow_identity",
		"billing":  "_grizzle_shadow_billing",
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "qualified table and fk",
			input: `CREATE TABLE identity.users (
				id BIGSERIAL PRIMARY KEY,
				email TEXT NOT NULL
			);
			CREATE TABLE billing.accounts (
				id BIGSERIAL PRIMARY KEY,
				user_id BIGINT REFERENCES identity.users(id),
				notes TEXT DEFAULT 'schema identity.users should not change'
			);`,
			expected: `CREATE TABLE _grizzle_shadow_identity.users (
				id BIGSERIAL PRIMARY KEY,
				email TEXT NOT NULL
			);
			CREATE TABLE _grizzle_shadow_billing.accounts (
				id BIGSERIAL PRIMARY KEY,
				user_id BIGINT REFERENCES _grizzle_shadow_identity.users(id),
				notes TEXT DEFAULT 'schema identity.users should not change'
			);`,
		},
		{
			name: "double quoted qualifiers",
			input: `CREATE TABLE "billing"."invoices" (
				id BIGSERIAL PRIMARY KEY,
				account_id BIGINT REFERENCES "billing"."accounts"(id)
			);`,
			expected: `CREATE TABLE "_grizzle_shadow_billing"."invoices" (
				id BIGSERIAL PRIMARY KEY,
				account_id BIGINT REFERENCES "_grizzle_shadow_billing"."accounts"(id)
			);`,
		},
		{
			name: "create schema if not exists",
			input: `CREATE SCHEMA IF NOT EXISTS billing;
CREATE SCHEMA identity;`,
			expected: `CREATE SCHEMA IF NOT EXISTS _grizzle_shadow_billing;
CREATE SCHEMA _grizzle_shadow_identity;`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := postgres.RewriteShadowSQL(tt.input, shadowMap)
			if got != tt.expected {
				t.Errorf("RewriteShadowSQL mismatch:\ngot:\n%s\nwant:\n%s", got, tt.expected)
			}
		})
	}
}

func TestComputeShadowSchemas(t *testing.T) {
	// Single schema public
	mSingle := postgres.ComputeShadowSchemas("", []string{"public"})
	if mSingle["public"] != "_grizzle_shadow" {
		t.Errorf("expected _grizzle_shadow for single public schema, got %q", mSingle["public"])
	}

	// Multi schema
	mMulti := postgres.ComputeShadowSchemas("", []string{"public", "billing", "identity"})
	if mMulti["public"] != "_grizzle_shadow_public" {
		t.Errorf("expected _grizzle_shadow_public, got %q", mMulti["public"])
	}
	if mMulti["billing"] != "_grizzle_shadow_billing" {
		t.Errorf("expected _grizzle_shadow_billing, got %q", mMulti["billing"])
	}
	if mMulti["identity"] != "_grizzle_shadow_identity" {
		t.Errorf("expected _grizzle_shadow_identity, got %q", mMulti["identity"])
	}
}

func TestComputeShadowSchemas_LongNamesBoundedAndDistinct(t *testing.T) {
	a := strings.Repeat("a", 60)
	b := strings.Repeat("b", 60)
	m := postgres.ComputeShadowSchemas("_grizzle_shadow", []string{a, b})
	if m[a] == m[b] {
		t.Fatalf("long target schemas collapsed to same shadow name %q", m[a])
	}
	for target, shadow := range m {
		if len(shadow) > 63 {
			t.Errorf("shadow for %q exceeds 63 bytes: len=%d name=%q", target, len(shadow), shadow)
		}
		if shadow == target {
			t.Errorf("shadow must never equal target %q", target)
		}
	}
}
