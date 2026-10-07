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
