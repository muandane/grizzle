package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/plan"
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

func TestWrapShadowCompileError_ImmutableIndex(t *testing.T) {
	origErr := errors.New("functions in index expression must be marked IMMUTABLE")
	sqlStr := `
		CREATE TABLE orders (id BIGINT, created_at TIMESTAMPTZ);
		CREATE INDEX idx_orders_created ON orders ((clock_timestamp()));
	`

	wrapped := postgres.WrapShadowCompileError(origErr, sqlStr)
	if wrapped == nil {
		t.Fatal("expected non-nil error")
	}
	msg := wrapped.Error()
	if !strings.Contains(msg, `index "idx_orders_created" on table "orders"`) {
		t.Errorf("expected index and table names in error, got: %s", msg)
	}
	if !strings.Contains(msg, "expression must be IMMUTABLE") {
		t.Errorf("expected 'expression must be IMMUTABLE' in error, got: %s", msg)
	}
	if !errors.Is(wrapped, origErr) {
		t.Errorf("expected wrapped error to unwrap to original error")
	}

	// SQL without CREATE INDEX statement
	wrappedNoStmt := postgres.WrapShadowCompileError(origErr, "")
	if !strings.Contains(wrappedNoStmt.Error(), "index expression must be IMMUTABLE") {
		t.Errorf("expected 'index expression must be IMMUTABLE', got: %s", wrappedNoStmt.Error())
	}
}

func TestWrapShadowCompileError_PartitionKey(t *testing.T) {
	t.Run("table in error", func(t *testing.T) {
		origErr := errors.New(`ERROR: unique constraint on partitioned table "measurements" must include all partitioning columns (SQLSTATE 0A000)`)
		wrapped := postgres.WrapShadowCompileError(origErr, "")
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `table "measurements"`) {
			t.Errorf("expected table 'measurements' in error, got: %s", msg)
		}
		if !errors.Is(wrapped, plan.ErrPartitionKeyNotInUnique) {
			t.Errorf("expected ErrPartitionKeyNotInUnique, got: %v", wrapped)
		}
		if !errors.Is(wrapped, origErr) {
			t.Errorf("expected wrapped error to match original error")
		}
	})

	t.Run("table in SQL", func(t *testing.T) {
		origErr := errors.New("primary key on partitioned table must include all partitioning columns")
		sqlStr := `CREATE TABLE events (id BIGINT, event_date DATE, PRIMARY KEY (id)) PARTITION BY RANGE (event_date);`
		wrapped := postgres.WrapShadowCompileError(origErr, sqlStr)
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `table "events"`) {
			t.Errorf("expected table 'events' in error, got: %s", msg)
		}
		if !errors.Is(wrapped, plan.ErrPartitionKeyNotInUnique) {
			t.Errorf("expected ErrPartitionKeyNotInUnique, got: %v", wrapped)
		}
	})

	t.Run("no table parseable", func(t *testing.T) {
		origErr := errors.New("unique constraint on partitioned table must include all partitioning columns")
		wrapped := postgres.WrapShadowCompileError(origErr, "")
		if !errors.Is(wrapped, plan.ErrPartitionKeyNotInUnique) {
			t.Errorf("expected ErrPartitionKeyNotInUnique, got: %v", wrapped)
		}
	})
}

func TestWrapShadowCompileError_UndefinedType_MissingExtension(t *testing.T) {
	cases := []struct {
		name     string
		typeStr  string
		sqlStr   string
		errStr   string
		wantStmt string
	}{
		{
			name:     "citext in table",
			typeStr:  "citext",
			sqlStr:   "CREATE TABLE users (id BIGSERIAL PRIMARY KEY, email citext NOT NULL);",
			errStr:   `ERROR: type "citext" does not exist (SQLSTATE 42704)`,
			wantStmt: `table "users"`,
		},
		{
			name:     "vector unqualified",
			typeStr:  "vector",
			sqlStr:   "",
			errStr:   `type "vector" does not exist`,
			wantStmt: "",
		},
		{
			name:     "geometry in table",
			typeStr:  "geometry",
			sqlStr:   "CREATE TABLE places (id INT, location geometry(Point, 4326));",
			errStr:   `type "geometry" does not exist`,
			wantStmt: `table "places"`,
		},
		{
			name:     "hstore unqualified",
			typeStr:  "hstore",
			sqlStr:   "",
			errStr:   `type "hstore" does not exist`,
			wantStmt: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origErr := errors.New(tc.errStr)
			wrapped := postgres.WrapShadowCompileError(origErr, tc.sqlStr)
			if wrapped == nil {
				t.Fatal("expected non-nil error")
			}
			msg := wrapped.Error()
			if !strings.Contains(msg, fmt.Sprintf(`type %q`, tc.typeStr)) {
				t.Errorf("expected type %q in error, got: %s", tc.typeStr, msg)
			}
			if !strings.Contains(msg, "hint: missing extension or type definition") {
				t.Errorf("expected missing extension or type definition hint, got: %s", msg)
			}
			if tc.wantStmt != "" && !strings.Contains(msg, tc.wantStmt) {
				t.Errorf("expected statement %q in error, got: %s", tc.wantStmt, msg)
			}
			if !errors.Is(wrapped, origErr) {
				t.Errorf("expected original error unwrapped")
			}
		})
	}

	t.Run("pgconn PgError with Code 42704", func(t *testing.T) {
		driverErr := &pgconn.PgError{
			Code:         "42704",
			Message:      `type "citext" does not exist`,
			DataTypeName: "citext",
		}
		wrapped := postgres.WrapShadowCompileError(driverErr, "CREATE TABLE users (email citext);")
		var pgErr *pgconn.PgError
		if !errors.As(wrapped, &pgErr) {
			t.Fatalf("expected errors.As to succeed with *pgconn.PgError, got: %v", wrapped)
		}
		if pgErr.Code != "42704" {
			t.Errorf("expected pgErr.Code 42704, got: %s", pgErr.Code)
		}
	})
}

func TestWrapShadowCompileError_UndefinedFunction(t *testing.T) {
	t.Run("uuid_generate_v4 in default", func(t *testing.T) {
		origErr := errors.New("function uuid_generate_v4() does not exist")
		sqlStr := "CREATE TABLE items (id UUID DEFAULT uuid_generate_v4(), name TEXT);"
		wrapped := postgres.WrapShadowCompileError(origErr, sqlStr)
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `table "items"`) {
			t.Errorf("expected table 'items' in error, got: %s", msg)
		}
		if !strings.Contains(msg, `function "uuid_generate_v4()"`) {
			t.Errorf("expected function 'uuid_generate_v4()' in error, got: %s", msg)
		}
		if !strings.Contains(msg, "missing extension") || !strings.Contains(msg, "uuid-ossp") || !strings.Contains(msg, "function definition") {
			t.Errorf("expected missing extension hint mentioning 'uuid-ossp' or function definition, got: %s", msg)
		}
	})

	t.Run("gen_random_uuid standalone", func(t *testing.T) {
		origErr := errors.New(`ERROR: function "gen_random_uuid"() does not exist (SQLSTATE 42883)`)
		wrapped := postgres.WrapShadowCompileError(origErr, "")
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `function "gen_random_uuid()"`) {
			t.Errorf("expected function name in error, got: %s", msg)
		}
		if !strings.Contains(msg, "missing extension") || !strings.Contains(msg, "function definition") {
			t.Errorf("expected hint mentioning missing extension or function definition, got: %s", msg)
		}
	})
}

func TestWrapShadowCompileError_UndefinedRelation_ForeignKeyOrView(t *testing.T) {
	t.Run("foreign key referencing missing table", func(t *testing.T) {
		origErr := errors.New(`relation "users" does not exist`)
		sqlStr := `
			CREATE TABLE orders (
				id BIGSERIAL PRIMARY KEY,
				user_id BIGINT REFERENCES users(id)
			);
		`
		wrapped := postgres.WrapShadowCompileError(origErr, sqlStr)
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `table "orders"`) {
			t.Errorf("expected failing table 'orders' in error, got: %s", msg)
		}
		if !strings.Contains(msg, `relation "users"`) {
			t.Errorf("expected missing relation 'users' in error, got: %s", msg)
		}
		if !strings.Contains(msg, "table creation order") || !strings.Contains(msg, "schema qualification") {
			t.Errorf("expected hint mentioning table creation order or schema qualification, got: %s", msg)
		}
	})

	t.Run("view referencing missing relation", func(t *testing.T) {
		origErr := errors.New(`ERROR: relation "public"."accounts" does not exist (SQLSTATE 42P01)`)
		sqlStr := `CREATE VIEW active_accounts AS SELECT * FROM accounts WHERE active = true;`
		wrapped := postgres.WrapShadowCompileError(origErr, sqlStr)
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `view "active_accounts"`) {
			t.Errorf("expected view name in error, got: %s", msg)
		}
		if !strings.Contains(msg, `relation "accounts"`) {
			t.Errorf("expected relation 'accounts' in error, got: %s", msg)
		}
		if !strings.Contains(msg, "table creation order") || !strings.Contains(msg, "schema qualification") {
			t.Errorf("expected hint mentioning table creation order or schema qualification, got: %s", msg)
		}
	})

	t.Run("standalone relation error without SQL", func(t *testing.T) {
		origErr := errors.New(`relation "tenants" does not exist`)
		wrapped := postgres.WrapShadowCompileError(origErr, "")
		msg := wrapped.Error()
		if !strings.Contains(msg, `relation "tenants"`) {
			t.Errorf("expected relation name, got: %s", msg)
		}
		if !strings.Contains(msg, "table creation order") {
			t.Errorf("expected table creation order hint, got: %s", msg)
		}
	})
}

func TestWrapShadowCompileError_UndefinedColumn(t *testing.T) {
	t.Run("index referencing missing column", func(t *testing.T) {
		origErr := errors.New(`column "user_uuid" does not exist`)
		sqlStr := `
			CREATE TABLE accounts (id BIGINT, user_id BIGINT);
			CREATE INDEX idx_accounts_user ON accounts (user_uuid);
		`
		wrapped := postgres.WrapShadowCompileError(origErr, sqlStr)
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `index "idx_accounts_user" on table "accounts"`) {
			t.Errorf("expected index and table name, got: %s", msg)
		}
		if !strings.Contains(msg, `column "user_uuid"`) {
			t.Errorf("expected column 'user_uuid', got: %s", msg)
		}
		if !strings.Contains(msg, "column name spelling") || !strings.Contains(msg, "declaration order") {
			t.Errorf("expected hint mentioning column name spelling or declaration order, got: %s", msg)
		}
	})

	t.Run("column of relation in error message", func(t *testing.T) {
		origErr := errors.New(`column "tenant_id" of relation "orders" does not exist`)
		wrapped := postgres.WrapShadowCompileError(origErr, "")
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `table "orders"`) {
			t.Errorf("expected table 'orders' in error, got: %s", msg)
		}
		if !strings.Contains(msg, `column "tenant_id"`) {
			t.Errorf("expected column 'tenant_id' in error, got: %s", msg)
		}
		if !strings.Contains(msg, "column name spelling") || !strings.Contains(msg, "declaration order") {
			t.Errorf("expected hint mentioning column name spelling or declaration order, got: %s", msg)
		}
	})
}

func TestWrapShadowCompileError_DuplicateObject(t *testing.T) {
	t.Run("relation already exists", func(t *testing.T) {
		origErr := errors.New(`relation "users" already exists`)
		sqlStr := `CREATE TABLE users (id INT); CREATE TABLE users (id INT);`
		wrapped := postgres.WrapShadowCompileError(origErr, sqlStr)
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `relation "users"`) {
			t.Errorf("expected relation 'users', got: %s", msg)
		}
		if !strings.Contains(msg, "hint: duplicate definition") {
			t.Errorf("expected duplicate definition hint, got: %s", msg)
		}
	})

	t.Run("type already exists", func(t *testing.T) {
		origErr := errors.New(`type "status_enum" already exists`)
		wrapped := postgres.WrapShadowCompileError(origErr, "CREATE TYPE status_enum AS ENUM ('active');")
		if wrapped == nil {
			t.Fatal("expected non-nil error")
		}
		msg := wrapped.Error()
		if !strings.Contains(msg, `type "status_enum"`) {
			t.Errorf("expected type 'status_enum', got: %s", msg)
		}
		if !strings.Contains(msg, "hint: duplicate definition") {
			t.Errorf("expected duplicate definition hint, got: %s", msg)
		}
	})
}

func TestWrapShadowCompileError_FallbackAndOpaque(t *testing.T) {
	t.Run("syntax error in single statement", func(t *testing.T) {
		origErr := errors.New(`syntax error at or near "BADTOKEN"`)
		sqlStr := `CREATE TABLE bad_table (id INT BADTOKEN);`
		wrapped := postgres.WrapShadowCompileError(origErr, sqlStr)
		msg := wrapped.Error()
		if !strings.Contains(msg, `table "bad_table"`) {
			t.Errorf("expected statement 'bad_table' surfaced in fallback, got: %s", msg)
		}
	})

	t.Run("opaque error with empty SQL", func(t *testing.T) {
		origErr := errors.New("server closed connection unexpectedly")
		wrapped := postgres.WrapShadowCompileError(origErr, "")
		if wrapped.Error() != origErr.Error() {
			t.Errorf("expected opaque error unmodified, got: %s", wrapped.Error())
		}
	})

	t.Run("nil error", func(t *testing.T) {
		if err := postgres.WrapShadowCompileError(nil, "CREATE TABLE t (id INT);"); err != nil {
			t.Errorf("expected nil error for nil input, got: %v", err)
		}
	})
}

type mockFailingDBTX struct {
	execErr error
}

func (m *mockFailingDBTX) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.HasPrefix(strings.TrimSpace(query), "SET") {
		return nil, nil
	}
	return nil, m.execErr
}

func (m *mockFailingDBTX) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return nil, m.execErr
}

func (m *mockFailingDBTX) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return nil
}

func TestRunShadowDDL_ErrorWrapping(t *testing.T) {
	mockErr := errors.New(`type "citext" does not exist`)
	dbtx := &mockFailingDBTX{execErr: mockErr}
	sqlStr := "CREATE TABLE users (email citext);"

	err := postgres.RunShadowDDL(context.Background(), dbtx, "test_shadow", "public", sqlStr)
	if err == nil {
		t.Fatal("expected RunShadowDDL to fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "schema compilation in shadow schema failed:") {
		t.Errorf("expected 'schema compilation in shadow schema failed' prefix, got: %s", msg)
	}
	if !strings.Contains(msg, `table "users"`) {
		t.Errorf("expected table 'users' in error, got: %s", msg)
	}
	if !strings.Contains(msg, `type "citext"`) {
		t.Errorf("expected type 'citext' in error, got: %s", msg)
	}
	if !strings.Contains(msg, "hint: missing extension or type definition") {
		t.Errorf("expected hint in error, got: %s", msg)
	}
}

func TestRunMultiShadowDDL_ErrorWrapping(t *testing.T) {
	mockErr := errors.New(`relation "users" does not exist`)
	dbtx := &mockFailingDBTX{execErr: mockErr}
	sqlStr := "CREATE TABLE orders (user_id BIGINT REFERENCES users(id));"
	shadowMap := map[string]string{"public": "_grizzle_shadow_public"}

	err := postgres.RunMultiShadowDDL(context.Background(), dbtx, shadowMap, []string{"public"}, sqlStr)
	if err == nil {
		t.Fatal("expected RunMultiShadowDDL to fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "schema compilation in shadow schema failed:") {
		t.Errorf("expected 'schema compilation in shadow schema failed' prefix, got: %s", msg)
	}
	if !strings.Contains(msg, `table "orders"`) {
		t.Errorf("expected table 'orders' in error, got: %s", msg)
	}
	if !strings.Contains(msg, `relation "users"`) {
		t.Errorf("expected relation 'users' in error, got: %s", msg)
	}
	if !strings.Contains(msg, "hint: verify table creation order or schema qualification") {
		t.Errorf("expected hint in error, got: %s", msg)
	}
}

type mockRecordingDBTX struct {
	queries []string
}

func (m *mockRecordingDBTX) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	m.queries = append(m.queries, query)
	return nil, nil
}

func (m *mockRecordingDBTX) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return nil, nil
}

func (m *mockRecordingDBTX) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return nil
}

func TestRunShadowDDL_RewritesTargetSchemaQualifiers(t *testing.T) {
	dbtx := &mockRecordingDBTX{}
	sqlStr := `CREATE FUNCTION public.check_payment_allocations_total() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END; $$;
CREATE TABLE public.customers (id uuid PRIMARY KEY);
CREATE TRIGGER tg_customers_handle_updated_at BEFORE UPDATE ON public.customers FOR EACH ROW EXECUTE FUNCTION public.handle_updated_at();
ALTER TABLE public.customers ENABLE ROW LEVEL SECURITY;
CREATE POLICY customer_org_isolation ON public.customers USING (true);`

	err := postgres.RunShadowDDL(context.Background(), dbtx, "_grizzle_shadow", "public", sqlStr)
	if err != nil {
		t.Fatalf("expected RunShadowDDL to succeed, got %v", err)
	}

	if len(dbtx.queries) < 3 {
		t.Fatalf("expected at least 3 queries, got %d", len(dbtx.queries))
	}

	execSQL := dbtx.queries[1]
	if strings.Contains(execSQL, "public.check_payment_allocations_total") {
		t.Errorf("execSQL still contains 'public.check_payment_allocations_total': %s", execSQL)
	}
	if !strings.Contains(execSQL, "_grizzle_shadow.check_payment_allocations_total") {
		t.Errorf("execSQL missing '_grizzle_shadow.check_payment_allocations_total': %s", execSQL)
	}
	if strings.Contains(execSQL, "public.customers") {
		t.Errorf("execSQL still contains 'public.customers': %s", execSQL)
	}
	if !strings.Contains(execSQL, "_grizzle_shadow.customers") {
		t.Errorf("execSQL missing '_grizzle_shadow.customers': %s", execSQL)
	}
	if !strings.Contains(execSQL, "ON _grizzle_shadow.customers") {
		t.Errorf("execSQL missing 'ON _grizzle_shadow.customers': %s", execSQL)
	}
	if !strings.Contains(execSQL, "EXECUTE FUNCTION _grizzle_shadow.handle_updated_at") {
		t.Errorf("execSQL missing 'EXECUTE FUNCTION _grizzle_shadow.handle_updated_at': %s", execSQL)
	}
}

func TestRewriteShadowSQL_SingleSchemaQualified(t *testing.T) {
	shadowMap := map[string]string{
		"public": "_grizzle_shadow",
	}

	input := `CREATE FUNCTION public.check_payment_allocations_total() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN RETURN NULL; END; $$;
COMMENT ON FUNCTION public.document_invoice_payable_minor(p_currency text) IS 'Invoice amount';
CREATE TABLE public.customer_portal_members (
    id uuid NOT NULL,
    organization_id uuid REFERENCES public.organizations(id)
);
CREATE FUNCTION public.lookup_portal() RETURNS SETOF public.customer_portal_members
    AS $$ SELECT * FROM public.customer_portal_members; $$;
CREATE TABLE "public"."quoted_table" (
    id INT REFERENCES "public"."other"(id)
);
CREATE VIEW PUBLIC.active_view AS SELECT * FROM public.customer_portal_members;
COMMENT ON COLUMN public.customer_portal_members.id IS 'ID col';
CREATE POLICY org_isolation ON public.customer_portal_members TO public USING (true);`

	expected := `CREATE FUNCTION _grizzle_shadow.check_payment_allocations_total() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN RETURN NULL; END; $$;
COMMENT ON FUNCTION _grizzle_shadow.document_invoice_payable_minor(p_currency text) IS 'Invoice amount';
CREATE TABLE _grizzle_shadow.customer_portal_members (
    id uuid NOT NULL,
    organization_id uuid REFERENCES _grizzle_shadow.organizations(id)
);
CREATE FUNCTION _grizzle_shadow.lookup_portal() RETURNS SETOF _grizzle_shadow.customer_portal_members
    AS $$ SELECT * FROM _grizzle_shadow.customer_portal_members; $$;
CREATE TABLE "_grizzle_shadow"."quoted_table" (
    id INT REFERENCES "_grizzle_shadow"."other"(id)
);
CREATE VIEW _grizzle_shadow.active_view AS SELECT * FROM _grizzle_shadow.customer_portal_members;
COMMENT ON COLUMN _grizzle_shadow.customer_portal_members.id IS 'ID col';
CREATE POLICY org_isolation ON _grizzle_shadow.customer_portal_members TO public USING (true);`

	got := postgres.RewriteShadowSQL(input, shadowMap)
	if got != expected {
		t.Errorf("RewriteShadowSQL mismatch:\ngot:\n%s\nwant:\n%s", got, expected)
	}
}

func TestRewriteShadowSQL_NovaSchema(t *testing.T) {
	novaPath := "/Users/zine/Developer/simplified/sanad/apps/nova/internal/db/schema/schema.sql"
	data, err := os.ReadFile(novaPath)
	if err != nil {
		t.Skipf("skipping local Nova schema test: %v", err)
	}

	shadowMap := map[string]string{
		"public": "_grizzle_shadow",
	}

	rewritten := postgres.RewriteShadowSQL(string(data), shadowMap)

	// Specifically verify that public.check_payment_allocations_total() is rewritten
	if strings.Contains(rewritten, "public.check_payment_allocations_total") {
		t.Errorf("rewritten Nova schema still contains 'public.check_payment_allocations_total'")
	}
	if !strings.Contains(rewritten, "_grizzle_shadow.check_payment_allocations_total") {
		t.Errorf("rewritten Nova schema missing '_grizzle_shadow.check_payment_allocations_total'")
	}

	// Verify tables are rewritten
	if strings.Contains(rewritten, "CREATE TABLE public.customer_portal_members") {
		t.Errorf("rewritten Nova schema still contains 'CREATE TABLE public.customer_portal_members'")
	}
	if !strings.Contains(rewritten, "CREATE TABLE _grizzle_shadow.customer_portal_members") {
		t.Errorf("rewritten Nova schema missing 'CREATE TABLE _grizzle_shadow.customer_portal_members'")
	}

	// Verify triggers are rewritten
	if strings.Contains(rewritten, "EXECUTE FUNCTION public.handle_updated_at") {
		t.Errorf("rewritten Nova schema still contains 'EXECUTE FUNCTION public.handle_updated_at'")
	}
	if !strings.Contains(rewritten, "EXECUTE FUNCTION _grizzle_shadow.handle_updated_at") {
		t.Errorf("rewritten Nova schema missing 'EXECUTE FUNCTION _grizzle_shadow.handle_updated_at'")
	}
}
