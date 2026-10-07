//go:build integration

package exec_test

import (
	"context"
	"testing"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestCompileSchemaPostgres(t *testing.T) {
	db := testutil.TestDatabase(t)

	schemaSQL := `
		CREATE TABLE users (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email TEXT NOT NULL
		);
		CREATE TABLE posts (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			user_id INTEGER REFERENCES users (id)
		);`

	s, err := exec.CompileSchemaPostgres(context.Background(), db, exec.PostgresExecConfig{
		TargetSchema: "public",
		SchemaSQL:    schemaSQL,
	})
	if err != nil {
		t.Fatalf("CompileSchemaPostgres failed: %v", err)
	}

	if s.Name != "public" {
		t.Errorf("schema name = %q, want %q", s.Name, "public")
	}
	users, ok := s.Tables["users"]
	if !ok {
		t.Fatalf("users table missing from compiled IR: %+v", s.Tables)
	}
	if users.PrimaryKey == nil || len(users.PrimaryKey.Columns) != 1 || users.PrimaryKey.Columns[0] != "id" {
		t.Errorf("users primary key not introspected: %+v", users.PrimaryKey)
	}
	if users.Columns["id"] == nil || !users.Columns["id"].IsIdentity {
		t.Errorf("users.id identity flag not introspected: %+v", users.Columns["id"])
	}
	posts, ok := s.Tables["posts"]
	if !ok {
		t.Fatalf("posts table missing from compiled IR: %+v", s.Tables)
	}
	if len(posts.ForeignKeys) != 1 {
		t.Errorf("posts foreign keys = %d, want 1", len(posts.ForeignKeys))
	}

	// The compilation transaction must leave no shadow schema behind.
	var shadowCount int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_namespace WHERE nspname LIKE '\_grizzle\_shadow%'`,
	).Scan(&shadowCount); err != nil {
		t.Fatalf("querying shadow schemas: %v", err)
	}
	if shadowCount != 0 {
		t.Errorf("shadow schema leaked: %d found", shadowCount)
	}
}

func TestCompileSchemaPostgres_CompileError(t *testing.T) {
	db := testutil.TestDatabase(t)

	if _, err := exec.CompileSchemaPostgres(context.Background(), db, exec.PostgresExecConfig{
		TargetSchema: "public",
		SchemaSQL:    "CREATE TABLE broken (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,; )",
	}); err == nil {
		t.Fatal("expected compile error for invalid DDL, got nil")
	}
}
