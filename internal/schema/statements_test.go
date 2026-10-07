package schema

import (
	"strings"
	"testing"
)

func TestSplitStatements_BasicAndQuoting(t *testing.T) {
	sql := `CREATE TABLE a (id int); -- comment; with semicolon
CREATE TABLE b (name text); /* block; comment */
CREATE TABLE c (v text DEFAULT 'it''s; tricky');
CREATE TABLE d (body text DEFAULT $$dollar; inside$$);
CREATE TABLE e (x int);
CREATE TABLE "semi"";colon" (x int);`
	stmts := SplitStatements(sql)
	if len(stmts) != 6 {
		t.Fatalf("expected 6 statements, got %d: %v", len(stmts), stmts)
	}
	for _, s := range stmts {
		if strings.Contains(s, "-- comment") {
			// comments are preserved verbatim inside statements; fine
			_ = s
		}
	}
	if !strings.Contains(stmts[2], "it''s; tricky") {
		t.Errorf("single-quoted semicolon split incorrectly: %s", stmts[2])
	}
	if !strings.Contains(stmts[3], "dollar; inside") {
		t.Errorf("dollar-quoted semicolon split incorrectly: %s", stmts[3])
	}
	if !strings.Contains(stmts[5], `"semi"";colon"`) {
		t.Errorf("escaped quoted identifier was split incorrectly: %s", stmts[5])
	}
}

func TestSplitStatements_EmptyAndTrailing(t *testing.T) {
	if got := SplitStatements(""); len(got) != 0 {
		t.Errorf("empty input should yield no statements, got %v", got)
	}
	if got := SplitStatements("  ;  ; "); len(got) != 0 {
		t.Errorf("only-semicolon input should yield no statements, got %v", got)
	}
	got := SplitStatements("SELECT 1;")
	if len(got) != 1 || got[0] != "SELECT 1" {
		t.Errorf("trailing statement lost: %v", got)
	}
}

func TestParseExtensions(t *testing.T) {
	sql := `CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION citext WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS "MyExt" WITH SCHEMA my_schema;
CREATE TABLE users (id int);
CREATE EXTENSION vector;`
	exts := ParseExtensions(sql)
	want := []string{"citext", "myext", "pgcrypto", "vector"}
	if len(exts) != 4 {
		t.Fatalf("expected 4 extensions, got %d: %+v", len(exts), exts)
	}
	for _, name := range want {
		if _, ok := exts[name]; !ok {
			t.Errorf("missing extension %q in %+v", name, exts)
		}
	}
	if exts["citext"].Schema != "public" {
		t.Errorf("citext schema = %q, want public", exts["citext"].Schema)
	}
	if exts["myext"].Schema != "my_schema" {
		t.Errorf("MyExt schema = %q, want my_schema", exts["myext"].Schema)
	}
	if exts["vector"].Schema != "" {
		t.Errorf("vector schema = %q, want empty", exts["vector"].Schema)
	}
}

func TestParseExtensions_None(t *testing.T) {
	sql := "CREATE TABLE users (id int); CREATE INDEX idx ON users(id);"
	if exts := ParseExtensions(sql); len(exts) != 0 {
		t.Errorf("expected no extensions, got %+v", exts)
	}
}

func TestStripExtensionStatements(t *testing.T) {
	sql := `CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE users (id int, email citext);
CREATE EXTENSION citext;
CREATE INDEX idx_users_email ON users(email);`
	cleaned, exts := StripExtensionStatements(sql)
	if len(exts) != 2 {
		t.Fatalf("expected 2 stripped extension statements, got %d: %v", len(exts), exts)
	}
	if strings.Contains(cleaned, "CREATE EXTENSION") {
		t.Errorf("cleaned SQL still contains CREATE EXTENSION: %s", cleaned)
	}
	if !strings.Contains(cleaned, "CREATE TABLE users") || !strings.Contains(cleaned, "CREATE INDEX idx_users_email") {
		t.Errorf("cleaned SQL lost DDL: %s", cleaned)
	}
}

func TestStripExtensionStatements_All(t *testing.T) {
	cleaned, exts := StripExtensionStatements("CREATE EXTENSION pgcrypto;")
	if cleaned != "" || len(exts) != 1 {
		t.Errorf("expected empty cleaned + 1 ext, got %q / %v", cleaned, exts)
	}
}

func TestFindDMLStatements(t *testing.T) {
	sql := `CREATE TABLE users (id int);
INSERT INTO users VALUES (1);
insert into users values (2);
UPDATE users SET id = 3;
DELETE FROM users WHERE id = 3;
TRUNCATE users;
CREATE INDEX idx ON users(id);
-- INSERT comment only`
	dml := FindDMLStatements(sql)
	if len(dml) != 5 {
		t.Fatalf("expected 5 DML statements, got %d: %v", len(dml), dml)
	}
}

func TestExtractStatements_MixedSchemaSQL(t *testing.T) {
	sql := `-- unified desired state
CREATE TABLE docs (body text DEFAULT 'CREATE ROLE not_a_role;');
/* The semicolon below is comment text; it must not split. */
CREATE ROLE app_read;
GRANT SELECT ON docs TO app_read;
ALTER ROLE app_read SET search_path = public;
REVOKE INSERT ON docs FROM app_read;
CREATE PUBLICATION docs_pub FOR TABLE docs;
CREATE EVENT TRIGGER audit_ddl ON ddl_command_end EXECUTE FUNCTION log_ddl();
ALTER PUBLICATION docs_pub ADD TABLE audit;
DROP EVENT TRIGGER old_audit;
ALTER TABLE docs ADD COLUMN title text;
DROP ROLE should_stay_shadow;`

	groups := ExtractStatements(sql)
	if !strings.Contains(groups.ShadowSQL, "CREATE TABLE docs") ||
		!strings.Contains(groups.ShadowSQL, "ALTER TABLE docs") {
		t.Fatalf("shadow SQL lost unsupported/DDL statements: %q", groups.ShadowSQL)
	}
	if strings.Contains(groups.ShadowSQL, "CREATE ROLE app_read") ||
		strings.Contains(groups.ShadowSQL, "CREATE PUBLICATION docs_pub") {
		t.Fatalf("side-channel statements leaked into shadow SQL: %q", groups.ShadowSQL)
	}
	if !strings.Contains(groups.RolesSQL, "CREATE ROLE app_read") ||
		!strings.Contains(groups.RolesSQL, "GRANT SELECT ON docs TO app_read") ||
		!strings.Contains(groups.RolesSQL, "ALTER ROLE app_read") ||
		!strings.Contains(groups.RolesSQL, "REVOKE INSERT ON docs FROM app_read") ||
		!strings.Contains(groups.RolesSQL, "DROP ROLE should_stay_shadow") {
		t.Fatalf("role statements not extracted: %q", groups.RolesSQL)
	}
	if !strings.Contains(groups.CatalogSQL, "CREATE PUBLICATION docs_pub") ||
		!strings.Contains(groups.CatalogSQL, "CREATE EVENT TRIGGER audit_ddl") ||
		!strings.Contains(groups.CatalogSQL, "ALTER PUBLICATION docs_pub") ||
		!strings.Contains(groups.CatalogSQL, "DROP EVENT TRIGGER old_audit") {
		t.Fatalf("catalog statements not extracted: %q", groups.CatalogSQL)
	}
}

func TestExtractStatements_PreservesCommentsAndQuotedStrings(t *testing.T) {
	sql := `/* CREATE ROLE fake; */
CREATE TABLE docs (
  body text DEFAULT 'CREATE PUBLICATION fake; -- not SQL'
);
-- CREATE EVENT TRIGGER fake;
CREATE ROLE app_read;`

	groups := ExtractStatements(sql)
	if !strings.Contains(groups.ShadowSQL, "CREATE PUBLICATION fake; -- not SQL") {
		t.Fatalf("quoted string was altered or split: %q", groups.ShadowSQL)
	}
	if !strings.Contains(groups.RolesSQL, "-- CREATE EVENT TRIGGER fake;") ||
		!strings.Contains(groups.RolesSQL, "CREATE ROLE app_read") {
		t.Fatalf("leading comments were not preserved with extracted statement: %q", groups.RolesSQL)
	}
	if strings.Contains(groups.CatalogSQL, "fake") {
		t.Fatalf("comment/string contents were misclassified as catalog SQL: %q", groups.CatalogSQL)
	}
}
