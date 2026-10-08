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
		{`CREATE ROLE password;`, ""},
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
			if tt.message == "" {
				if err != nil {
					t.Fatalf("valid role identifier %q was rejected: %v", tt.sql, err)
				}
				return
			}
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

func TestRoleIdentity_PublicPseudoGranteeDiffersFromRealRole(t *testing.T) {
	if !IsPublicRoleIdentifier("PUBLIC") || IsPublicRoleIdentifier(`"public"`) {
		t.Fatal("PUBLIC keyword and quoted public role must have distinct identities")
	}
	if GrantKey("TABLE", "docs", "PUBLIC") == GrantKey("TABLE", "docs", `"public"`) {
		t.Fatal("PUBLIC pseudo-grantee must not share a key with real role public")
	}
	if GrantKey("TABLE", "docs", "app") != GrantKey("TABLE", "docs", `"app"`) {
		t.Fatal("quoted and unquoted lower-case role identifiers should match")
	}
}

func TestMergeRolesSpecs_QualifiesTargetsBeforeSideOverlay(t *testing.T) {
	schemaSpec := ParseRolesSQL(`GRANT SELECT ON docs TO app;`)
	sideSpec := ParseRolesSQL(`GRANT INSERT ON public.docs TO app;`)
	merged := MergeRolesSpecsForTarget(schemaSpec, sideSpec, "public")
	if len(merged.Grants) != 1 || len(merged.Grants[0].Privileges) != 1 ||
		merged.Grants[0].Privileges[0] != "INSERT" {
		t.Fatalf("qualified and unqualified duplicate targets must overlay: %+v", merged.Grants)
	}
}

func TestMergeRolesSpecs_QualifiesMixedCaseTargetBeforeSideOverlay(t *testing.T) {
	schemaSpec := ParseRolesSQL(`GRANT SELECT ON docs TO app;`)
	sideSpec := ParseRolesSQL(`GRANT INSERT ON "MixedSchema"."docs" TO app;`)
	merged := MergeRolesSpecsForTarget(schemaSpec, sideSpec, "MixedSchema")
	if len(merged.Grants) != 1 || len(merged.Grants[0].Privileges) != 1 ||
		merged.Grants[0].Privileges[0] != "INSERT" {
		t.Fatalf("mixed-case qualified and unqualified targets must overlay: %+v", merged.Grants)
	}
}

func TestCanonicalGrantObject_PreservesQuotedDotsAndQuotes(t *testing.T) {
	object := `"schema.with.dot"."table""name"`
	if got, want := CanonicalGrantObject("TABLE", object, "public"), `"schema.with.dot"."table""name"`; got != want {
		t.Fatalf("CanonicalGrantObject(%q) = %q, want %q", object, got, want)
	}
}

func TestCanonicalGrantObject_NormalizesFunctionTypeAliases(t *testing.T) {
	tests := []struct {
		object string
		want   string
	}{
		{object: `touch(int)`, want: `public.touch(integer)`},
		{object: `touch(int4, bool, varchar, timestamptz)`, want: `public.touch(integer, boolean, character varying, timestamp with time zone)`},
		{object: `touch(int8[])`, want: `public.touch(bigint[])`},
	}
	for _, tt := range tests {
		if got := CanonicalGrantObject("FUNCTION", tt.object, "public"); got != tt.want {
			t.Errorf("CanonicalGrantObject(%q) = %q, want %q", tt.object, got, tt.want)
		}
	}
}

func TestCanonicalGrantObject_PreservesMixedCaseTargetSchema(t *testing.T) {
	if got, want := CanonicalGrantObject("TABLE", "docs", "MixedSchema"), `"MixedSchema".docs`; got != want {
		t.Fatalf("mixed-case target schema = %q, want %q", got, want)
	}
	if got, want := CanonicalGrantObject("TABLE", `"MixedSchema"."Order"`, "MixedSchema"), `"MixedSchema"."Order"`; got != want {
		t.Fatalf("quoted mixed-case target/table = %q, want %q", got, want)
	}
}

func TestValidateRolesSQL_RejectsOverlengthIdentifiers(t *testing.T) {
	longName := strings.Repeat("r", 64)
	if err := ValidateRolesSQL(`CREATE ROLE "` + longName + `";`); err == nil {
		t.Fatal("role identifiers over PostgreSQL's 63-byte limit must be rejected")
	}
}

func TestValidateRolesSpecScope_RejectsUninspectedObjects(t *testing.T) {
	spec := ParseRolesSQL(`
		GRANT SELECT ON TABLE other.docs TO app;
		GRANT USAGE ON SCHEMA other TO app;
		GRANT CONNECT ON DATABASE other_db TO app;
	`)
	if err := ValidateRolesSpecScope(spec, []string{"public"}, "app_db"); err == nil {
		t.Fatal("ACL objects outside the inspected target/current-database scopes must be rejected")
	}
	if err := ValidateRolesSpecScope(ParseRolesSQL(`GRANT CONNECT ON DATABASE app_db TO app;`), []string{"public"}, "app_db"); err != nil {
		t.Fatalf("current database ACL should be accepted: %v", err)
	}
	if err := ValidateRolesSpecScope(ParseRolesSQL(`GRANT SELECT ON TABLE "Mixed"."docs" TO app;`), []string{"Mixed"}, "app_db"); err != nil {
		t.Fatalf("quoted target schema should be accepted: %v", err)
	}
}

func TestFilterRolePrivilegesForServer_Maintain(t *testing.T) {
	spec := ParseRolesSQL(`GRANT ALL ON TABLE docs TO app;`)
	if len(spec.Grants) != 1 || !containsFold(spec.Grants[0].Privileges, "MAINTAIN") {
		t.Fatalf("ALL TABLE must include MAINTAIN in the desired expansion: %+v", spec.Grants)
	}
	if _, err := FilterRolePrivilegesForServer(spec, 160000); err != nil {
		t.Fatalf("ALL expansion should remain compatible on PostgreSQL 16: %v", err)
	}
	if containsFold(spec.Grants[0].Privileges, "MAINTAIN") {
		t.Fatal("MAINTAIN must be omitted for PostgreSQL versions before 17")
	}
	spec = ParseRolesSQL(`GRANT ALL ON TABLE docs TO app;`)
	if _, err := FilterRolePrivilegesForServer(spec, 170000); err != nil {
		t.Fatalf("MAINTAIN should be accepted on PostgreSQL 17: %v", err)
	}
	if !containsFold(spec.Grants[0].Privileges, "MAINTAIN") {
		t.Fatal("MAINTAIN must remain on PostgreSQL 17+")
	}
	explicit := ParseRolesSQL(`GRANT MAINTAIN ON TABLE docs TO app;`)
	if _, err := FilterRolePrivilegesForServer(explicit, 160000); err == nil {
		t.Fatal("explicit MAINTAIN must be rejected before PostgreSQL 17")
	}
	explicitRevoke := ParseRolesSQL(`REVOKE MAINTAIN ON TABLE docs FROM app;`)
	if _, err := FilterRolePrivilegesForServer(explicitRevoke, 160000); err == nil {
		t.Fatal("explicit MAINTAIN revoke must be rejected before PostgreSQL 17")
	}
}
