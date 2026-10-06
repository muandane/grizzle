package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func TestPartialAndFunctionalIndexes_PureUnit(t *testing.T) {
	t.Parallel()

	filters := scope.Filters{}

	// 1. Functional index normalization (live has type casts and extra parens as returned by pg_get_indexdef)
	live := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "bigint", Position: 1},
					"email": {Name: "email", DataType: "text", Position: 2},
				},
				Indexes: map[string]*schema.Index{
					"idx_users_email_lower": {
						Name:       "idx_users_email_lower",
						TableName:  "users",
						IsUnique:   true,
						IsValid:    true,
						Definition: "CREATE UNIQUE INDEX idx_users_email_lower ON public.users USING btree (lower((email)::text))",
					},
					"idx_users_active_only": {
						Name:       "idx_users_active_only",
						TableName:  "users",
						IsUnique:   false,
						IsValid:    true,
						Predicate:  "((status)::text = 'active'::text)",
						Definition: "CREATE INDEX idx_users_active_only ON public.users USING btree (id) WHERE ((status)::text = 'active'::text)",
					},
				},
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	// Desired compiled in shadow schema
	desired := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "bigint", Position: 1},
					"email": {Name: "email", DataType: "text", Position: 2},
				},
				Indexes: map[string]*schema.Index{
					"idx_users_email_lower": {
						Name:       "idx_users_email_lower",
						TableName:  "users",
						IsUnique:   true,
						IsValid:    true,
						Definition: "CREATE UNIQUE INDEX idx_users_email_lower ON _shadow.users USING btree (lower((email)::text))",
					},
					"idx_users_active_only": {
						Name:       "idx_users_active_only",
						TableName:  "users",
						IsUnique:   false,
						IsValid:    true,
						Predicate:  "((status)::text = 'active'::text)",
						Definition: "CREATE INDEX idx_users_active_only ON _shadow.users USING btree (id) WHERE ((status)::text = 'active'::text)",
					},
				},
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes, err := diff.Diff(live, desired, "public", "_shadow", filters)
	if err != nil {
		t.Fatalf("unexpected diff error: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected 0 changes for identical normalized index definitions, got %d: %+v", len(changes), changes)
	}

	// 2. Modifying partial index WHERE predicate triggers DROP + CREATE
	desiredModifiedPred := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "bigint", Position: 1},
					"email": {Name: "email", DataType: "text", Position: 2},
				},
				Indexes: map[string]*schema.Index{
					"idx_users_email_lower": {
						Name:       "idx_users_email_lower",
						TableName:  "users",
						IsUnique:   true,
						IsValid:    true,
						Definition: "CREATE UNIQUE INDEX idx_users_email_lower ON _shadow.users USING btree (lower((email)::text))",
					},
					"idx_users_active_only": {
						Name:       "idx_users_active_only",
						TableName:  "users",
						IsUnique:   false,
						IsValid:    true,
						Predicate:  "((status)::text = 'verified'::text)",
						Definition: "CREATE INDEX idx_users_active_only ON _shadow.users USING btree (id) WHERE ((status)::text = 'verified'::text)",
					},
				},
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changesMod, err := diff.Diff(live, desiredModifiedPred, "public", "_shadow", filters)
	if err != nil {
		t.Fatalf("unexpected diff error: %v", err)
	}
	if len(changesMod) != 2 {
		t.Fatalf("expected 2 changes (DROP + CREATE) for modified predicate, got %d: %+v", len(changesMod), changesMod)
	}
	if changesMod[0].Type != plan.ChangeDropIndex || changesMod[0].Index.Name != "idx_users_active_only" {
		t.Errorf("expected DROP INDEX idx_users_active_only, got: %+v", changesMod[0])
	}
	if changesMod[1].Type != plan.ChangeCreateIndex || changesMod[1].Index.Name != "idx_users_active_only" {
		t.Errorf("expected CREATE INDEX idx_users_active_only, got: %+v", changesMod[1])
	}

	// 3. Modifying functional expression triggers DROP + CREATE
	desiredModifiedExpr := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "bigint", Position: 1},
					"email": {Name: "email", DataType: "text", Position: 2},
				},
				Indexes: map[string]*schema.Index{
					"idx_users_email_lower": {
						Name:       "idx_users_email_lower",
						TableName:  "users",
						IsUnique:   true,
						IsValid:    true,
						Definition: "CREATE UNIQUE INDEX idx_users_email_lower ON _shadow.users USING btree (upper((email)::text))",
					},
					"idx_users_active_only": {
						Name:       "idx_users_active_only",
						TableName:  "users",
						IsUnique:   false,
						IsValid:    true,
						Predicate:  "((status)::text = 'active'::text)",
						Definition: "CREATE INDEX idx_users_active_only ON _shadow.users USING btree (id) WHERE ((status)::text = 'active'::text)",
					},
				},
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changesExpr, err := diff.Diff(live, desiredModifiedExpr, "public", "_shadow", filters)
	if err != nil {
		t.Fatalf("unexpected diff error: %v", err)
	}
	if len(changesExpr) != 2 {
		t.Fatalf("expected 2 changes (DROP + CREATE) for modified functional expression, got %d: %+v", len(changesExpr), changesExpr)
	}
	if changesExpr[0].Type != plan.ChangeDropIndex || changesExpr[0].Index.Name != "idx_users_email_lower" {
		t.Errorf("expected DROP INDEX idx_users_email_lower, got: %+v", changesExpr[0])
	}
	if changesExpr[1].Type != plan.ChangeCreateIndex || changesExpr[1].Index.Name != "idx_users_email_lower" {
		t.Errorf("expected CREATE INDEX idx_users_email_lower, got: %+v", changesExpr[1])
	}
}

func TestPartialAndFunctionalIndexes_PostgresIntegration(t *testing.T) {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres partial/functional index test, db unavailable: %v", err)
	}

	ctx := context.Background()
	schemaPrefix := fmt.Sprintf("test_idx_pf_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// 1. Initial declarative schema containing:
	// - Functional index: lower(email)
	// - Partial index: status = 'pending'
	// - Combined functional + partial index: lower(username) WHERE is_active = true
	schemaSQL := `
		CREATE TABLE users (
			id BIGINT PRIMARY KEY,
			email TEXT NOT NULL
		);
		CREATE UNIQUE INDEX idx_users_email_lower ON users (lower(email));

		CREATE TABLE orders (
			id BIGINT PRIMARY KEY,
			created_at DATE NOT NULL,
			status TEXT NOT NULL
		);
		CREATE INDEX idx_orders_pending ON orders (created_at) WHERE status = 'pending';

		CREATE TABLE accounts (
			id BIGINT PRIMARY KEY,
			username TEXT NOT NULL,
			is_active BOOLEAN NOT NULL
		);
		CREATE UNIQUE INDEX idx_accounts_active_user ON accounts (lower(username)) WHERE is_active = true;
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    schemaSQL,
	}

	// 2. Sync to apply schema
	if err := grizzle.Sync(ctx, db, opts); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// 3. Verify steps created concurrently (NonTx)
	planDiff, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(planDiff.Steps) != 0 {
		t.Fatalf("expected 0 steps on re-diff (idempotency), got %d: %+v", len(planDiff.Steps), planDiff.Steps)
	}

	// 4. Modifying partial index WHERE predicate
	modifiedSQL := `
		CREATE TABLE users (
			id BIGINT PRIMARY KEY,
			email TEXT NOT NULL
		);
		CREATE UNIQUE INDEX idx_users_email_lower ON users (lower(email));

		CREATE TABLE orders (
			id BIGINT PRIMARY KEY,
			created_at DATE NOT NULL,
			status TEXT NOT NULL
		);
		CREATE INDEX idx_orders_pending ON orders (created_at) WHERE status = 'processing';

		CREATE TABLE accounts (
			id BIGINT PRIMARY KEY,
			username TEXT NOT NULL,
			is_active BOOLEAN NOT NULL
		);
		CREATE UNIQUE INDEX idx_accounts_active_user ON accounts (lower(username)) WHERE is_active = true;
	`
	optsModified := grizzle.Options{
		Dialect:        grizzle.DialectPostgres,
		TargetSchema:   schemaPrefix,
		SchemaSQL:      modifiedSQL,
		AllowDropIndex: new(true),
	}

	pMod, err := grizzle.PlanDiff(ctx, db, optsModified)
	if err != nil {
		t.Fatalf("PlanDiff on modified predicate failed: %v", err)
	}

	// Expect DROP INDEX + CREATE INDEX for idx_orders_pending
	var foundDrop, foundCreate bool
	for _, s := range pMod.Steps {
		if s.Type == grizzle.ChangeDropIndex && s.SQL != "" {
			foundDrop = true
			if !s.NonTx {
				t.Errorf("DROP INDEX step should be NonTx (CONCURRENTLY)")
			}
		}
		if s.Type == grizzle.ChangeCreateIndex && s.SQL != "" {
			foundCreate = true
			if !s.NonTx {
				t.Errorf("CREATE INDEX step should be NonTx (CONCURRENTLY)")
			}
		}
	}
	if !foundDrop || !foundCreate {
		t.Fatalf("expected DROP and CREATE index steps for modified predicate, got steps: %+v", pMod.Steps)
	}

	// Apply modified index
	if err := grizzle.Sync(ctx, db, optsModified); err != nil {
		t.Fatalf("Sync modified failed: %v", err)
	}

	// Verify idempotency after update
	pMod2, err := grizzle.PlanDiff(ctx, db, optsModified)
	if err != nil {
		t.Fatalf("PlanDiff re-verification failed: %v", err)
	}
	if len(pMod2.Steps) != 0 {
		t.Fatalf("expected 0 steps after modifying index, got %d: %+v", len(pMod2.Steps), pMod2.Steps)
	}

	// 5. Invalidation recovery: simulate failed CONCURRENTLY build leaving indisvalid = false
	//nolint:gosec // G201: test executes setup query on generated schema
	_, err = db.Exec(fmt.Sprintf(`
		UPDATE pg_index
		SET indisvalid = false
		WHERE indexrelid = '%s.idx_users_email_lower'::regclass;
	`, schemaPrefix))
	if err != nil {
		t.Fatalf("marking functional index as invalid failed: %v", err)
	}

	// PlanDiff must detect indisvalid = false and plan DROP INDEX CONCURRENTLY + CREATE INDEX CONCURRENTLY
	pRepair, err := grizzle.PlanDiff(ctx, db, optsModified)
	if err != nil {
		t.Fatalf("PlanDiff for invalid index repair failed: %v", err)
	}
	if len(pRepair.Steps) != 2 {
		t.Fatalf("expected 2 steps for invalid functional index repair, got %d: %+v", len(pRepair.Steps), pRepair.Steps)
	}
	if pRepair.Steps[0].Type != grizzle.ChangeDropIndex || !pRepair.Steps[0].NonTx {
		t.Errorf("step 0 should be non-tx DROP INDEX, got: %+v", pRepair.Steps[0])
	}
	if pRepair.Steps[1].Type != grizzle.ChangeCreateIndex || !pRepair.Steps[1].NonTx {
		t.Errorf("step 1 should be non-tx CREATE INDEX, got: %+v", pRepair.Steps[1])
	}

	// Repair using Sync
	if err := grizzle.Sync(ctx, db, optsModified); err != nil {
		t.Fatalf("Sync repair failed: %v", err)
	}

	// Verify repaired index is now valid and plan is empty
	pFinal, err := grizzle.PlanDiff(ctx, db, optsModified)
	if err != nil {
		t.Fatalf("PlanDiff after repair failed: %v", err)
	}
	if len(pFinal.Steps) != 0 {
		t.Fatalf("expected 0 steps after repair, got %d: %+v", len(pFinal.Steps), pFinal.Steps)
	}
}

func TestPartialAndFunctionalIndexes_SQLite_Parity(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	schemaSQL := `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			email TEXT NOT NULL,
			status TEXT NOT NULL
		);
		CREATE UNIQUE INDEX idx_users_email_lower ON users (lower(email));
		CREATE INDEX idx_users_active ON users (id) WHERE status = 'active';
	`

	opts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
	}

	if err := grizzle.Sync(ctx, db, opts); err != nil {
		t.Fatalf("Sync SQLite failed: %v", err)
	}

	planDiff, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("PlanDiff SQLite failed: %v", err)
	}
	if len(planDiff.Steps) != 0 {
		t.Fatalf("expected 0 steps on SQLite re-diff (idempotency), got %d: %+v", len(planDiff.Steps), planDiff.Steps)
	}
}

func TestPartialAndFunctionalIndexes_CustomFunction(t *testing.T) {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres custom function index test, db unavailable: %v", err)
	}

	ctx := context.Background()
	schemaPrefix := fmt.Sprintf("test_idx_fn_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	//nolint:gosec // G201: test executes setup query on generated schema
	setupSQL := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE OR REPLACE FUNCTION custom_hash(t text) RETURNS text AS $$
		BEGIN
			RETURN md5(t);
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;
	`, schemaPrefix)
	if _, err := db.Exec(setupSQL); err != nil {
		t.Fatalf("setup custom function failed: %v", err)
	}

	schemaSQL := `
		CREATE OR REPLACE FUNCTION custom_hash(t text) RETURNS text AS $$
		BEGIN
			RETURN md5(t);
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;

		CREATE TABLE documents (
			id BIGINT PRIMARY KEY,
			body TEXT NOT NULL
		);
		CREATE INDEX idx_docs_hash ON documents (custom_hash(body));
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    schemaSQL,
	}

	if err := grizzle.Sync(ctx, db, opts); err != nil {
		t.Fatalf("Sync custom function failed: %v", err)
	}

	p, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("PlanDiff custom function failed: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("expected 0 steps on re-diff with custom function index (idempotency), got %d: %+v", len(p.Steps), p.Steps)
	}
}

func TestPartialAndFunctionalIndexes_NonImmutableFunctionRejection(t *testing.T) {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres non-immutable function test, db unavailable: %v", err)
	}

	ctx := context.Background()
	schemaPrefix := fmt.Sprintf("test_idx_non_imm_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// Volatile function in index: now() or clock_timestamp()
	badSchemaSQL := `
		CREATE TABLE orders (
			id BIGINT PRIMARY KEY,
			amount NUMERIC(10,2) NOT NULL
		);
		CREATE INDEX idx_orders_created ON orders ((clock_timestamp()));
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    badSchemaSQL,
	}

	// 1. Sync must fail at shadow pre-flight
	err = grizzle.Sync(ctx, db, opts)
	if err == nil {
		t.Fatalf("expected Sync to fail when index uses non-IMMUTABLE function, got nil")
	}

	// 2. Error message must contain index name, table name, and "expression must be IMMUTABLE"
	errMsg := err.Error()
	if !strings.Contains(errMsg, "idx_orders_created") {
		t.Errorf("expected error message to contain index name 'idx_orders_created', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "orders") {
		t.Errorf("expected error message to contain table name 'orders', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "expression must be IMMUTABLE") {
		t.Errorf("expected error message to contain 'expression must be IMMUTABLE', got: %s", errMsg)
	}

	// 3. Assert real database schema is completely untouched
	var tableExists bool
	err = db.QueryRow(fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = '%s' AND table_name = 'orders');", schemaPrefix)).Scan(&tableExists)
	if err != nil {
		t.Fatalf("failed checking table existence: %v", err)
	}
	if tableExists {
		t.Errorf("expected orders table to NOT exist in target schema, but it was created")
	}
}

func TestPartialAndFunctionalIndexes_FunctionInNonTableNonShadowSchema(t *testing.T) {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres non-table schema function test, db unavailable: %v", err)
	}

	ctx := context.Background()
	timeTag := time.Now().UnixNano()
	utilsSchema := fmt.Sprintf("test_utils_%d", timeTag)
	targetSchema := fmt.Sprintf("test_app_%d", timeTag)

	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s; CREATE SCHEMA %s;", utilsSchema, targetSchema))
	if err != nil {
		t.Fatalf("failed creating schemas: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE; DROP SCHEMA %s CASCADE;", utilsSchema, targetSchema))
	}()

	// 1. Create an immutable function in the non-target utilsSchema
	createFuncSQL := fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s.hash_code(val text) RETURNS text AS $$
		BEGIN
			RETURN encode(digest(val, 'sha256'), 'hex');
		EXCEPTION WHEN undefined_function THEN
			RETURN md5(val);
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;
	`, utilsSchema)
	if _, err := db.Exec(createFuncSQL); err != nil {
		t.Fatalf("failed creating function in utils schema: %v", err)
	}

	// 2. Define table and functional index in targetSchema qualifying the external function
	schemaSQL := fmt.Sprintf(`
		CREATE TABLE items (
			id BIGINT PRIMARY KEY,
			code TEXT NOT NULL
		);
		CREATE INDEX idx_items_code_hash ON items (%s.hash_code(code));
	`, utilsSchema)

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: targetSchema,
		SchemaSQL:    schemaSQL,
	}

	// 3. Sync must succeed because the external function in utilsSchema is resolvable
	if err := grizzle.Sync(ctx, db, opts); err != nil {
		t.Fatalf("Sync failed with function in external schema: %v", err)
	}

	// 4. PlanDiff must detect 0 steps (roundtrip idempotency)
	planDiff, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(planDiff.Steps) != 0 {
		t.Fatalf("expected 0 steps on re-diff, got %d: %+v", len(planDiff.Steps), planDiff.Steps)
	}
}

func TestPartialAndFunctionalIndexes_RoundtripIdempotency_Normalizations(t *testing.T) {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed opening pg: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(); err != nil {
		t.Skipf("skipping postgres normalization test, db unavailable: %v", err)
	}

	ctx := context.Background()
	schemaPrefix := fmt.Sprintf("test_idx_norm_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// Schema with functional index with implicit text casts and WHERE predicate with mixed whitespace
	schemaSQL := `
		CREATE TABLE users (
			id BIGINT PRIMARY KEY,
			email TEXT NOT NULL,
			status VARCHAR(50) NOT NULL,
			score INT NOT NULL
		);
		CREATE UNIQUE INDEX idx_users_email_lower ON users (lower(email));
		CREATE INDEX idx_users_active ON users (id) WHERE status = 'active' AND score > 0;
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    schemaSQL,
	}

	// 1. Initial Sync
	if err := grizzle.Sync(ctx, db, opts); err != nil {
		t.Fatalf("initial Sync failed: %v", err)
	}

	// 2. Re-diff must produce zero steps (exact pg_get_indexdef normalization match)
	p, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("expected 0 steps on re-diff due to indexdef normalization, got %d: %+v", len(p.Steps), p.Steps)
	}
}
