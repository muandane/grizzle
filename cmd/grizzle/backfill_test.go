package main

import (
	"database/sql"
	"flag"
	"strings"
	"testing"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

func TestBuildCopyBackfillSQL(t *testing.T) {
	t.Parallel()

	pg := buildCopyBackfillSQL(grizzle.DialectPostgres, `"users"`, "id", "email_address", "email")
	if !strings.Contains(pg, `SET "email" = "email_address"`) {
		t.Fatalf("postgres SQL missing SET clause: %s", pg)
	}
	if !strings.Contains(pg, `"id" > $1`) || !strings.Contains(pg, `LIMIT $2`) {
		t.Fatalf("postgres SQL missing keyset placeholders: %s", pg)
	}
	if !strings.Contains(pg, `WHERE "id" IN (SELECT "id" FROM "users"`) {
		t.Fatalf("postgres SQL missing pk subquery: %s", pg)
	}

	sq := buildCopyBackfillSQL(grizzle.DialectSQLite, `"users"`, "id", "email_address", "email")
	if !strings.Contains(sq, `"id" > ?`) || !strings.Contains(sq, `LIMIT ?`) {
		t.Fatalf("sqlite SQL missing keyset placeholders: %s", sq)
	}

	first := buildCopyBackfillSQLFirstBatch(grizzle.DialectSQLite, `"users"`, "id", "a", "b")
	if strings.Contains(first, `> ?`) || strings.Contains(first, `> $1`) {
		t.Fatalf("first-batch SQL should omit cursor lower bound: %s", first)
	}
	if !strings.Contains(first, `LIMIT ?`) {
		t.Fatalf("first-batch SQL missing LIMIT: %s", first)
	}
}

func TestLookupSingleColumnPK_RequiresSingleColumn(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", "file:backfill_pk?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := t.Context()
	for _, stmt := range []string{
		`CREATE TABLE no_pk (name TEXT)`,
		`CREATE TABLE composite_pk (a INT, b INT, PRIMARY KEY (a, b))`,
		`CREATE TABLE single_pk (id INTEGER PRIMARY KEY, name TEXT)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := lookupSingleColumnPK(ctx, tx, grizzle.DialectSQLite, "no_pk"); err == nil {
		t.Fatal("expected error for table without primary key")
	} else if !strings.Contains(err.Error(), "single-column primary key") {
		t.Fatalf("unexpected error for no_pk: %v", err)
	}

	if _, err := lookupSingleColumnPK(ctx, tx, grizzle.DialectSQLite, "composite_pk"); err == nil {
		t.Fatal("expected error for composite primary key")
	} else if !strings.Contains(err.Error(), "single-column primary key") {
		t.Fatalf("unexpected error for composite_pk: %v", err)
	}

	got, err := lookupSingleColumnPK(ctx, tx, grizzle.DialectSQLite, "single_pk")
	if err != nil {
		t.Fatalf("single_pk lookup: %v", err)
	}
	if got != "id" {
		t.Fatalf("single_pk column = %q, want id", got)
	}
}

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
	if !strings.Contains(err.Error(), "single-column primary key") {
		t.Fatalf("unexpected error: %v", err)
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
