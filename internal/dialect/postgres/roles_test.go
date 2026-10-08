package postgres

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/schema"
)

func TestCanonicalLiveGrantObject_FunctionSignature(t *testing.T) {
	tests := []struct {
		schema, name, args, want string
	}{
		{schema: "public", name: "touch_ts", args: "", want: "public.touch_ts()"},
		{schema: "Public", name: "Touch_TS", args: "integer, text", want: `"Public"."Touch_TS"(integer, text)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalLiveGrantObject("FUNCTION", tt.schema+"."+tt.name+"("+tt.args+")")
			if got != tt.want {
				t.Fatalf("canonicalLiveGrantObject = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPostgresIdentifierRenderingEscapesAndPreservesQualifiedNames(t *testing.T) {
	if got, want := quoteIdentifier(`role"name`), `"role""name"`; got != want {
		t.Fatalf("quoteIdentifier = %q, want %q", got, want)
	}
	if got, want := GenerateCreateRoleSQL(&schema.Role{Name: `role"name`}), `CREATE ROLE "role""name" NOLOGIN;`; got != want {
		t.Fatalf("role renderer = %q, want %q", got, want)
	}
	grantSQL := GenerateGrantSQL(&schema.Grant{
		Grantee:    `"public"`,
		ObjectKind: "TABLE",
		ObjectName: `"schema.with.dot"."table""name"`,
		Privileges: []string{"SELECT"},
	})
	if want := `GRANT SELECT ON TABLE "schema.with.dot"."table""name" TO "public";`; grantSQL != want {
		t.Fatalf("quoted grant SQL = %q, want %q", grantSQL, want)
	}
	if got := canonicalLiveGrantObject("TABLE", `schema.with.dot.table"name`); got != `schema.with.dot."table""name"` {
		t.Fatalf("unquoted dotted live object should be structurally qualified: %q", got)
	}
	if got := canonicalLiveGrantObject("TABLE", `"schema.with.dot"."table""name"`); got != `"schema.with.dot"."table""name"` {
		t.Fatalf("quoted dotted live object = %q", got)
	}
}

func TestGenerateGrantSQL_NormalizesUnquotedIdentifierCase(t *testing.T) {
	sql := GenerateGrantSQL(&schema.Grant{
		Grantee:    "app",
		ObjectKind: "TABLE",
		ObjectName: "Docs",
		Privileges: []string{"SELECT"},
	})
	if !strings.Contains(sql, `ON TABLE "docs" TO "app"`) {
		t.Fatalf("unquoted identifiers must follow PostgreSQL lower-case semantics: %s", sql)
	}
}

func TestRoleManagedCommentRequiresExactMarker(t *testing.T) {
	if !roleManagedComment(schema.RoleManagedComment) {
		t.Fatal("exact managed marker must be recognized")
	}
	if roleManagedComment(schema.RoleManagedComment + "-extra") {
		t.Fatal("marker suffixes must not be treated as managed")
	}
}

func TestGenerateRoleSQL_DoublesMaliciousQuotes(t *testing.T) {
	roleName := `role"; DROP ROLE admin; --`
	if got, want := GenerateCreateRoleSQL(&schema.Role{Name: roleName}), `CREATE ROLE "role""; DROP ROLE admin; --" NOLOGIN;`; got != want {
		t.Fatalf("malicious role name rendering = %q, want %q", got, want)
	}
	grantSQL := GenerateGrantSQL(&schema.Grant{
		Grantee:    `"app""role"`,
		ObjectKind: "TABLE",
		ObjectName: `"schema""name"."table""name"`,
		Privileges: []string{"SELECT"},
	})
	if want := `GRANT SELECT ON TABLE "schema""name"."table""name" TO "app""role";`; grantSQL != want {
		t.Fatalf("malicious grant rendering = %q, want %q", grantSQL, want)
	}
}
