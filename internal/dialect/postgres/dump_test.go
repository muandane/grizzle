package postgres_test

import (
	"strings"
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/schema"
)

func TestPostgres_Dump(t *testing.T) {
	s := &schema.Schema{
		Name: "public",
		Extensions: map[string]*schema.Extension{
			"citext": {Name: "citext"},
		},
		Enums: map[string]*schema.Enum{
			"role": {Name: "role", Values: []string{"admin", "member"}},
		},
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":   {Name: "id", DataType: "bigint", Position: 1, IsIdentity: true, IdentityType: "ALWAYS"},
					"name": {Name: "name", DataType: "text", Position: 2, IsNullable: false},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "users_pkey", Columns: []string{"id"}},
			},
			"posts": {
				Name: "posts",
				Columns: map[string]*schema.Column{
					"id":      {Name: "id", DataType: "bigint", Position: 1, IsIdentity: true, IdentityType: "ALWAYS"},
					"user_id": {Name: "user_id", DataType: "bigint", Position: 2, IsNullable: false},
					"title":   {Name: "title", DataType: "text", Position: 3, IsNullable: false},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "posts_pkey", Columns: []string{"id"}},
				ForeignKeys: map[string]*schema.ForeignKey{
					"posts_user_id_fkey": {
						Name:       "posts_user_id_fkey",
						TableName:  "posts",
						RefTable:   "users",
						Definition: "FOREIGN KEY (user_id) REFERENCES users(id)",
					},
				},
				RLSEnabled: true,
			},
		},
	}

	ddl := postgres.Dump(s)

	if !strings.Contains(ddl, `CREATE EXTENSION IF NOT EXISTS "citext";`) {
		t.Errorf("expected extension citext in ddl, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `CREATE TYPE "public"."role" AS ENUM ('admin', 'member');`) {
		t.Errorf("expected enum in ddl, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `CREATE TABLE "public"."users"`) {
		t.Errorf("expected CREATE TABLE users in ddl, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `CREATE TABLE "public"."posts"`) {
		t.Errorf("expected CREATE TABLE posts in ddl, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `ALTER TABLE "public"."posts" ENABLE ROW LEVEL SECURITY;`) {
		t.Errorf("expected RLS enable in ddl, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `ALTER TABLE "public"."posts" ADD CONSTRAINT "posts_user_id_fkey" FOREIGN KEY (user_id) REFERENCES users(id);`) {
		t.Errorf("expected FK constraint in ddl, got:\n%s", ddl)
	}
}
