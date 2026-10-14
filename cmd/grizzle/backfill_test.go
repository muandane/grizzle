package main

import (
	"database/sql"
	"flag"
	"strings"
	"testing"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

func TestCopyBackfillHook_KeysetBatches(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", "file:backfill_copy?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := t.Context()
	_, err = db.ExecContext(ctx, `
CREATE TABLE members (id INTEGER PRIMARY KEY, email_address TEXT, email TEXT);
INSERT INTO members (id, email_address, email) VALUES
  (1, 'a@x', NULL), (2, 'b@x', NULL), (3, 'c@x', NULL), (4, 'd@x', NULL);
`)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	hook := newCopyBackfillHook(grizzle.DialectSQLite, 2)
	for range 3 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := hook(ctx, tx, "members", "email_address", "email"); err != nil {
			_ = tx.Rollback()
			t.Fatalf("hook: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	var nulls int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM members WHERE email IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls != 0 {
		t.Fatalf("expected all rows backfilled, still %d null", nulls)
	}
}

func TestCopyBackfillHook_RefusesNoPK(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", "file:backfill_nopk?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `CREATE TABLE items (name TEXT, old_c TEXT, new_c TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	hook := newCopyBackfillHook(grizzle.DialectSQLite, 10)
	err = hook(ctx, tx, "items", "old_c", "new_c")
	if err == nil {
		t.Fatal("expected PK refusal error")
	}
	if !strings.Contains(err.Error(), "requires a primary key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCopyBackfillHook_CompositePK(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", "file:cli_comp_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := t.Context()
	_, err = db.ExecContext(ctx, `
		CREATE TABLE tenant_items (
			tenant_id INT NOT NULL,
			item_id INT NOT NULL,
			old_c TEXT NOT NULL,
			new_c TEXT,
			PRIMARY KEY (tenant_id, item_id)
		);
		INSERT INTO tenant_items VALUES
			(1, 10, 'val1', NULL),
			(1, 20, 'val2', NULL),
			(2, 10, 'val3', NULL);
	`)
	if err != nil {
		t.Fatalf("create/insert: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	hook := newCopyBackfillHook(grizzle.DialectSQLite, 10)
	if err := hook(ctx, tx, "tenant_items", "old_c", "new_c"); err != nil {
		t.Fatalf("composite PK backfill hook failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var nulls int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenant_items WHERE new_c IS NULL`).Scan(&nulls)
	if nulls != 0 {
		t.Fatalf("expected 0 NULL rows, got %d", nulls)
	}
}

func TestBackfillFlag_Parse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bare", []string{"--backfill"}, "copy"},
		{"equals", []string{"--backfill=copy"}, "copy"},
		{"space", []string{"--backfill", "copy"}, "copy"},
		{"custom", []string{"--backfill=other"}, "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			var bf backfillFlag
			fs.Var(&bf, "backfill", "")
			if err := fs.Parse(normalizeBackfillArgs(tc.args)); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !bf.set || bf.strategy != tc.want {
				t.Fatalf("got set=%v strategy=%q, want %q", bf.set, bf.strategy, tc.want)
			}
		})
	}
}

func TestResolveBackfillHook_Validation(t *testing.T) {
	t.Parallel()

	var bf backfillFlag
	_ = bf.Set("copy")

	if _, err := resolveBackfillHook(false, bf, "", 1000, "sqlite::memory:"); err == nil {
		t.Fatal("expected error without --expand-contract")
	}

	bfFile := backfillFlag{}
	_ = bfFile.Set("copy")
	if _, err := resolveBackfillHook(true, bfFile, "x.sql", 1000, "sqlite::memory:"); err == nil {
		t.Fatal("expected conflict error for --backfill + --backfill-file")
	}

	bad := backfillFlag{}
	_ = bad.Set("merge")
	if _, err := resolveBackfillHook(true, bad, "", 1000, "sqlite::memory:"); err == nil {
		t.Fatal("expected unsupported strategy error")
	}

	hook, err := resolveBackfillHook(true, bf, "", 1000, "sqlite::memory:")
	if err != nil {
		t.Fatalf("copy strategy: %v", err)
	}
	if hook == nil {
		t.Fatal("expected non-nil hook")
	}
}

func TestApplyBackfillTemplate(t *testing.T) {
	t.Parallel()

	got := applyBackfillTemplate(
		`UPDATE {table} SET {new} = {old} WHERE {new} IS NULL LIMIT {batch}`,
		"users", "full_name", "display_name", 500,
	)
	want := `UPDATE "users" SET "display_name" = "full_name" WHERE "display_name" IS NULL LIMIT 500`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeBackfillArgs(t *testing.T) {
	t.Parallel()

	got := normalizeBackfillArgs([]string{"apply", "--expand-contract", "--backfill", "--dsn", "x"})
	want := []string{"apply", "--expand-contract", "--backfill=copy", "--dsn", "x"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}

	got = normalizeBackfillArgs([]string{"--backfill", "copy", "--backfill-batch", "10"})
	want = []string{"--backfill=copy", "--backfill-batch", "10"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCLI_BackfillRequiresExpandContract(t *testing.T) {
	if code := run([]string{"plan", "--dsn", "sqlite::memory:", "--backfill"}); code != 1 {
		t.Fatalf("expected exit 1 when --backfill without --expand-contract, got %d", code)
	}
}
