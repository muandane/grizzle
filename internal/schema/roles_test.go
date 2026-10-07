package schema

import (
	"strings"
	"testing"
)

func TestParseRolesSQL_Grants(t *testing.T) {
	spec := ParseRolesSQL(`
		CREATE ROLE app_read;
		CREATE USER app_writer WITH NOLOGIN;
		GRANT SELECT ON docs TO app_read;
		GRANT SELECT, INSERT ON private.docs, audit.docs TO app_writer WITH GRANT OPTION;
		GRANT ALL ON SEQUENCE docs_id_seq TO app_writer;
		GRANT CONNECT ON DATABASE mydb TO app_read;
		GRANT USAGE ON SCHEMA private TO app_read;
		GRANT EXECUTE ON FUNCTION touch_ts() TO app_read;
		GRANT SELECT ON docs TO PUBLIC;
		-- membership statements are not managed in v1
		GRANT app_read TO app_writer;
	`)

	if len(spec.Roles) != 2 {
		t.Fatalf("expected 2 roles, got %v", spec.Roles)
	}
	if spec.Roles["app_read"] == nil || spec.Roles["app_writer"] == nil {
		t.Fatalf("roles must be scanned: %v", spec.Roles)
	}

	byKey := make(map[string]*Grant)
	for _, g := range spec.Grants {
		byKey[GrantKey(g.ObjectKind, strings.ToLower(g.ObjectName), g.Grantee)] = g
	}

	g1 := byKey[GrantKey("TABLE", "docs", "APP_READ")]
	if g1 == nil || len(g1.Privileges) != 1 || g1.Privileges[0] != "SELECT" || g1.GrantOption {
		t.Fatalf("simple grant mismatch: %+v", g1)
	}

	g2 := byKey[GrantKey("TABLE", "private.docs", "APP_WRITER")]
	if g2 == nil || len(g2.Privileges) != 2 || !g2.GrantOption {
		t.Fatalf("multi-privilege grant with option mismatch: %+v", g2)
	}
	g2b := byKey[GrantKey("TABLE", "audit.docs", "APP_WRITER")]
	if g2b == nil {
		t.Fatalf("multi-object grant must expand: %+v", byKey)
	}

	g3 := byKey[GrantKey("SEQUENCE", "docs_id_seq", "APP_WRITER")]
	if g3 == nil || len(g3.Privileges) != 3 {
		t.Fatalf("ALL on sequence must expand to USAGE/SELECT/UPDATE: %+v", g3)
	}

	if byKey[GrantKey("DATABASE", "mydb", "APP_READ")] == nil {
		t.Fatalf("database grant must be scanned: %+v", byKey)
	}
	if byKey[GrantKey("SCHEMA", "private", "APP_READ")] == nil {
		t.Fatalf("schema grant must be scanned: %+v", byKey)
	}
	if byKey[GrantKey("FUNCTION", "touch_ts()", "APP_READ")] == nil {
		t.Fatalf("function grant must be scanned: %+v", byKey)
	}
	if byKey[GrantKey("TABLE", "docs", "PUBLIC")] == nil {
		t.Fatalf("PUBLIC grant must be scanned: %+v", byKey)
	}
}

func TestValidateRolesSQL_RejectsUnsupportedStatements(t *testing.T) {
	if err := ValidateRolesSQL("CREATE ROLE app_read; GRANT SELECT ON docs TO app_read;"); err != nil {
		t.Fatalf("valid roles SQL must pass: %v", err)
	}
	if err := ValidateRolesSQL("CREATE TABLE docs (id int);"); err == nil {
		t.Fatalf("schema DDL in RolesSQL must be rejected")
	}
	if err := ValidateRolesSQL("GRANT SELECT FROM docs TO app;"); err == nil {
		t.Fatalf("malformed GRANT must be rejected")
	}
	if err := ValidateRolesSQL("-- only comments\n"); err != nil {
		t.Fatalf("comment-only file must pass: %v", err)
	}
}

func TestValidateRolesSQL_UnifiedForms(t *testing.T) {
	valid := `
		CREATE ROLE app_read;
		GRANT SELECT ON docs TO app_read;
		REVOKE INSERT ON docs FROM app_read;
	`
	if err := ValidateRolesSQL(valid); err != nil {
		t.Fatalf("unified role statements must pass: %v", err)
	}
}

func TestValidateRolesSQL_RejectsUnsupportedRoleForms(t *testing.T) {
	tests := []struct {
		sql     string
		message string
	}{
		{`ALTER ROLE app_read SET search_path = public;`, "role configuration is not supported yet"},
		{`CREATE ROLE app_read PASSWORD 'secret';`, "passworded roles are not supported yet"},
		{`DROP ROLE app_read;`, "DROP ROLE/USER is not supported"},
		{`REVOKE GRANT OPTION FOR SELECT ON docs FROM app_read;`, "per-privilege grant-option revocation"},
		{`GRANT app_read TO app_writer WITH ADMIN OPTION;`, "unsupported GRANT form"},
		{`GRANT SELECT ON VIEW docs TO app_read;`, "object list contains malformed"},
		{`GRANT SELECT, ON docs TO app_read;`, "privilege list is malformed"},
		{`GRANT SELECT ON docs, TO app_read;`, "object list is malformed"},
		{`GRANT SELECT ON docs TO app_read,;`, "grantee list is malformed"},
		{`GRANT  ON docs TO app_read;`, "unsupported GRANT form"},
		{`GRANT SELECT ON FUNCTION fn(integer,) TO app_read;`, "object list contains malformed"},
		{`GRANT SELECT ON FUNCTION ""() TO app_read;`, "object list contains malformed"},
		{`CREATE ROLE "";`, "role identifier must not be empty"},
		{`GRANT SELECT ON docs TO "";`, "grantee list contains malformed"},
		{`GRANT SELECT ON docs TO app_read -- trailing comment`, "SQL comment"},
	}
	for _, tt := range tests {
		t.Run(tt.message, func(t *testing.T) {
			err := ValidateRolesSQL(tt.sql)
			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("ValidateRolesSQL(%q) = %v, want error containing %q", tt.sql, err, tt.message)
			}
		})
	}
}

func TestCanonicalGrantObject_FunctionIdentity(t *testing.T) {
	tests := []struct {
		name, object, want string
	}{
		{"unqualified", `touch_ts()`, `public.touch_ts()`},
		{"qualified quoted", `"Public"."Touch_TS"(integer, text)`, `"Public"."Touch_TS"(integer, text)`},
		{"overload", `touch_ts(integer, text)`, `public.touch_ts(integer, text)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanonicalGrantObject("FUNCTION", tt.object, "public"); got != tt.want {
				t.Fatalf("CanonicalGrantObject(%q) = %q, want %q", tt.object, got, tt.want)
			}
		})
	}
}

func TestParseRolesSQL_PreservesQuotedRoleIdentity(t *testing.T) {
	spec := ParseRolesSQL(`
		CREATE ROLE app;
		CREATE ROLE "App";
		GRANT SELECT ON docs TO app;
		GRANT SELECT ON docs TO "App";
	`)
	if len(spec.Roles) != 2 || len(spec.Grants) != 2 {
		t.Fatalf("quoted and unquoted role identities must remain distinct: roles=%v grants=%v", spec.Roles, spec.Grants)
	}
	if spec.Roles["app"] == nil || spec.Roles["App"] == nil {
		t.Fatalf("unexpected role identities: %v", spec.Roles)
	}
	if GrantKey("TABLE", "docs", spec.Grants[0].Grantee) ==
		GrantKey("TABLE", "docs", spec.Grants[1].Grantee) {
		t.Fatalf("quoted and unquoted grantees must not share an ACL key: %+v", spec.Grants)
	}
}

func TestParseRolesSQL_RevokeWinsDeclaratively(t *testing.T) {
	spec := ParseRolesSQL(`
		CREATE ROLE app_read;
		GRANT SELECT, INSERT ON docs TO app_read;
		REVOKE INSERT ON docs FROM app_read;
	`)
	if len(spec.Grants) != 1 || len(spec.Grants[0].Privileges) != 1 || spec.Grants[0].Privileges[0] != "SELECT" {
		t.Fatalf("REVOKE should remove the privilege from desired IR: %+v", spec.Grants)
	}
}

func TestMergeRolesSpecs_SideChannelWinsDuplicateGrant(t *testing.T) {
	schemaSpec := ParseRolesSQL(`CREATE ROLE app_read; GRANT SELECT ON docs TO app_read;`)
	sideSpec := ParseRolesSQL(`CREATE ROLE app_read; GRANT INSERT ON docs TO app_read;`)
	merged := MergeRolesSpecs(schemaSpec, sideSpec)
	if len(merged.Roles) != 1 || len(merged.Grants) != 1 {
		t.Fatalf("unexpected merged role spec: %+v", merged)
	}
	if len(merged.Grants[0].Privileges) != 1 || merged.Grants[0].Privileges[0] != "INSERT" {
		t.Fatalf("side-channel grant should replace duplicate SchemaSQL grant: %+v", merged.Grants[0])
	}

	merged = MergeRolesSpecs(schemaSpec, ParseRolesSQL(`REVOKE SELECT ON docs FROM app_read;`))
	if len(merged.Grants) != 0 {
		t.Fatalf("side-channel REVOKE should override duplicate SchemaSQL grant: %+v", merged.Grants)
	}

	schemaRevoked := ParseRolesSQL(`CREATE ROLE app_read; GRANT SELECT ON docs TO app_read; REVOKE SELECT ON docs FROM app_read;`)
	merged = MergeRolesSpecs(schemaRevoked, ParseRolesSQL(`GRANT SELECT ON docs TO app_read;`))
	if len(merged.Grants) != 1 || len(merged.Grants[0].Privileges) != 1 || merged.Grants[0].Privileges[0] != "SELECT" {
		t.Fatalf("side-channel GRANT should override SchemaSQL REVOKE: %+v", merged.Grants)
	}
}
