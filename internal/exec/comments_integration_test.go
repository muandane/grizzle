//go:build integration

package exec_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/scope"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestComments_Lifecycle verifies declarative COMMENT ON management: set,
// edit, no-op re-sync, and clear against real PostgreSQL.
func TestComments_Lifecycle(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_comments_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating test schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	newCfg := func(sql string) exec.PostgresExecConfig {
		return exec.PostgresExecConfig{
			TargetSchema:     schema,
			SchemaSQL:        sql,
			Filters:          scope.Filters{},
			Policy:           plan.DropPolicy{},
			LockTimeout:      5 * time.Second,
			StatementTimeout: 30 * time.Second,
		}
	}
	tblDesc := fmt.Sprintf("'%s'::regclass", schema+".docs")

	base := `CREATE TABLE docs (
		id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		title text NOT NULL
	);
	COMMENT ON TABLE docs IS 'docs table v1';
	COMMENT ON COLUMN docs.title IS 'the title';`

	// 1. Greenfield sync with comments.
	if err := exec.SyncPostgres(ctx, db, newCfg(base)); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	var tblComment, colComment string
	if err := db.QueryRow(fmt.Sprintf("SELECT obj_description(%s, 'pg_class')", tblDesc)).Scan(&tblComment); err != nil {
		t.Fatalf("read table comment: %v", err)
	}
	if tblComment != "docs table v1" {
		t.Fatalf("table comment = %q", tblComment)
	}
	if err := db.QueryRow(fmt.Sprintf("SELECT col_description(%s, 2)", tblDesc)).Scan(&colComment); err != nil {
		t.Fatalf("read column comment: %v", err)
	}
	if colComment != "the title" {
		t.Fatalf("column comment = %q", colComment)
	}

	// 2. Edit comments — replace, not alter.
	edited := `CREATE TABLE docs (
		id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		title text NOT NULL
	);
	COMMENT ON TABLE docs IS 'docs table v2';
	COMMENT ON COLUMN docs.title IS 'renamed note';`
	if err := exec.SyncPostgres(ctx, db, newCfg(edited)); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if err := db.QueryRow(fmt.Sprintf("SELECT obj_description(%s, 'pg_class')", tblDesc)).Scan(&tblComment); err != nil {
		t.Fatal(err)
	}
	if tblComment != "docs table v2" {
		t.Fatalf("edited table comment = %q", tblComment)
	}

	// 3. No-op re-sync.
	if err := exec.SyncPostgres(ctx, db, newCfg(edited)); err != nil {
		t.Fatalf("sync 3 (no-op): %v", err)
	}

	// 4. Clear comments (desired carries none).
	bare := `CREATE TABLE docs (
		id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		title text NOT NULL
	);`
	if err := exec.SyncPostgres(ctx, db, newCfg(bare)); err != nil {
		t.Fatalf("sync 4 (clear): %v", err)
	}
	if err := db.QueryRow(fmt.Sprintf("SELECT COALESCE(obj_description(%s, 'pg_class'), '')", tblDesc)).Scan(&tblComment); err != nil {
		t.Fatal(err)
	}
	if tblComment != "" {
		t.Fatalf("comment not cleared: %q", tblComment)
	}
}
