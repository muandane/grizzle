package lint_test

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/lint"
	"github.com/muandane/grizzle/internal/schema"
)

func TestNoDMLStatements_L009(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		wantDiags int
		wantKind  string // leading DML keyword of the first diagnostic, if any
	}{
		{
			name:      "clean DDL passes",
			sql:       "CREATE TABLE t (id bigint); CREATE INDEX idx ON t (id);",
			wantDiags: 0,
		},
		{
			name:      "insert flagged",
			sql:       "CREATE TABLE t (id bigint); INSERT INTO t VALUES (1);",
			wantDiags: 1,
			wantKind:  "INSERT",
		},
		{
			name:      "update flagged",
			sql:       "UPDATE t SET id = 2;",
			wantDiags: 1,
			wantKind:  "UPDATE",
		},
		{
			name:      "delete flagged",
			sql:       "DELETE FROM t;",
			wantDiags: 1,
			wantKind:  "DELETE",
		},
		{
			name:      "truncate flagged",
			sql:       "TRUNCATE t;",
			wantDiags: 1,
			wantKind:  "TRUNCATE",
		},
		{
			name:      "lowercase flagged",
			sql:       "insert into t values (1);",
			wantDiags: 1,
			wantKind:  "INSERT",
		},
		{
			name:      "commented-out DML not flagged",
			sql:       "-- INSERT INTO t VALUES (1);\n/* DELETE FROM t; */\nCREATE TABLE t (id bigint);",
			wantDiags: 0,
		},
		{
			name:      "DML inside dollar-quoted function body not flagged",
			sql:       "CREATE FUNCTION f() RETURNS void AS $$ INSERT INTO t VALUES (1); $$ LANGUAGE sql;",
			wantDiags: 0,
		},
		{
			name:      "DML inside single-quoted view body not flagged",
			sql:       "CREATE VIEW v AS SELECT * FROM t WHERE id IN (SELECT id FROM t);",
			wantDiags: 0,
		},
		{
			name:      "multiple DML statements each flagged",
			sql:       "INSERT INTO t VALUES (1); UPDATE t SET id = 2; DELETE FROM t;",
			wantDiags: 3,
			wantKind:  "DELETE",
		},
		{
			name:      "logical slot SELECT exempt from L009",
			sql:       "CREATE TABLE t (id bigint); SELECT pg_create_logical_replication_slot('s', 'pgoutput');",
			wantDiags: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &schema.Schema{Name: "public", SourceSQL: tt.sql}
			diags := lint.Lint(s, lint.NoDMLStatements{})
			if len(diags) != tt.wantDiags {
				t.Fatalf("expected %d diagnostics, got %d: %+v", tt.wantDiags, len(diags), diags)
			}
			if tt.wantDiags > 0 {
				if diags[0].RuleID != "L009" {
					t.Fatalf("expected L009, got %s", diags[0].RuleID)
				}
				if diags[0].Severity != lint.SeverityError {
					t.Fatalf("L009 must be ERROR, got %s", diags[0].Severity)
				}
				if !strings.HasPrefix(diags[0].Message, tt.wantKind) {
					t.Fatalf("expected message to lead with %q, got %q", tt.wantKind, diags[0].Message)
				}
			}
		})
	}
}
