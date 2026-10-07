//go:build integration

package exec_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestDryRunVerify_Postgres_NoMutation(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := fmt.Sprintf("test_dryrun_%d", time.Now().UnixNano())
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE %s.users (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email TEXT NOT NULL
		);
		INSERT INTO %s.users (email) VALUES ('a@b.c'), ('d@e.f');
	`, schema, schema)); err != nil {
		t.Fatalf("seeding live schema: %v", err)
	}

	desired := `
		CREATE TABLE users (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email TEXT NOT NULL
		);
		CREATE TABLE posts (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			user_id INTEGER REFERENCES users (id),
			title TEXT NOT NULL
		);
		CREATE INDEX idx_posts_user_id ON posts (user_id);
	`

	before, err := postgres.Inspect(ctx, db, schema)
	if err != nil {
		t.Fatalf("inspecting before: %v", err)
	}

	res, err := exec.DryRunVerifyPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL:    desired,
	})
	if err != nil {
		t.Fatalf("DryRunVerifyPostgres failed: %v", err)
	}
	if res.ExecutedSteps == 0 {
		t.Error("expected transactional steps to execute in the sandbox")
	}
	if res.Duration <= 0 {
		t.Error("expected positive duration")
	}

	after, err := postgres.Inspect(ctx, db, schema)
	if err != nil {
		t.Fatalf("inspecting after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("schema catalog changed after dry-run:\nbefore: %+v\nafter:  %+v", before, after)
	}

	// Data rows untouched.
	var userCount int
	if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s.users`, schema)).Scan(&userCount); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if userCount != 2 {
		t.Errorf("user rows = %d, want 2", userCount)
	}
}

func TestDryRunVerify_Postgres_UnverifiedNonTx(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := fmt.Sprintf("test_dryrun_ntx_%d", time.Now().UnixNano())
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}

	res, err := exec.DryRunVerifyPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL: `
			CREATE TABLE items (
				id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
				label TEXT NOT NULL
			);
			CREATE INDEX idx_items_label ON items (label);
		`,
	})
	if err != nil {
		t.Fatalf("DryRunVerifyPostgres failed: %v", err)
	}
	if len(res.UnverifiedNonTx) != 1 {
		t.Errorf("UnverifiedNonTx = %d, want 1 (CONCURRENTLY index)", len(res.UnverifiedNonTx))
	}
	if res.ExecutedSteps != 1 {
		t.Errorf("ExecutedSteps = %d, want 1 (CREATE TABLE)", res.ExecutedSteps)
	}
}

func TestDryRunVerify_Postgres_NotNullViolation(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := fmt.Sprintf("test_dryrun_chk_%d", time.Now().UnixNano())
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE %s.orders (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, note TEXT NOT NULL);
		INSERT INTO %s.orders (note) VALUES ('abc'), ('def');
	`, schema, schema)); err != nil {
		t.Fatalf("seeding live schema: %v", err)
	}

	// Negative plan: NOT NULL column addition without default against
	// existing rows (NOT_NULL_NO_DEFAULT is a critical hazard; accept it so
	// the dry-run reaches execution and fails against live rows).
	before, err := postgres.Inspect(ctx, db, schema)
	if err != nil {
		t.Fatalf("inspecting before: %v", err)
	}

	// Negative plan: cast note to INTEGER on non-numeric data would be
	// destructive, so verify a NOT NULL column addition with no default
	// instead (NOT_NULL_NO_DEFAULT is a critical hazard; accept it so the
	// dry-run reaches execution and fails against live rows).
	_, err = exec.DryRunVerifyPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL: `
			CREATE TABLE orders (
				id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
				note TEXT NOT NULL,
				status TEXT NOT NULL
			);
		`,
		AcceptHazards: []plan.HazardCode{plan.HazardNotNullNoDefault},
	})
	if err == nil {
		t.Fatal("expected dry-run to fail on integer cast of non-numeric live data")
	}
	if !strings.Contains(err.Error(), "dry-run verification failed at step") {
		t.Errorf("unexpected error: %v", err)
	}

	after, err := postgres.Inspect(ctx, db, schema)
	if err != nil {
		t.Fatalf("inspecting after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("schema catalog changed after failed dry-run")
	}
}

func TestDryRunVerify_Postgres_LockTimeout(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()
	schema := fmt.Sprintf("test_dryrun_lock_%d", time.Now().UnixNano())
	defer dropSchema(t, db, schema)

	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE %s.blocked (id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, note TEXT NOT NULL);
	`, schema)); err != nil {
		t.Fatalf("seeding live schema: %v", err)
	}

	// Hold an exclusive lock from a separate connection.
	blocker, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquiring blocker conn: %v", err)
	}
	defer func() { _ = blocker.Close() }()
	btx, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("beginning blocker tx: %v", err)
	}
	defer func() { _ = btx.Rollback() }()
	if _, err := btx.Exec(fmt.Sprintf(`LOCK TABLE %s.blocked IN ACCESS EXCLUSIVE MODE;`, schema)); err != nil {
		t.Fatalf("locking table: %v", err)
	}

	desired := `
		CREATE TABLE blocked (
			id INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			note TEXT NOT NULL,
			extra TEXT
		);
	`

	start := time.Now()
	_, err = exec.DryRunVerifyPostgres(ctx, db, exec.PostgresExecConfig{
		TargetSchema: schema,
		SchemaSQL:    desired,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected dry-run to fail on lock timeout")
	}
	if elapsed > 10*time.Second {
		t.Errorf("dry-run waited %s for locks; expected fail-fast", elapsed)
	}
	if err := btx.Rollback(); err != nil {
		t.Fatalf("releasing blocker: %v", err)
	}
}

func TestDryRunVerify_SQLite_NoMutationAndNotNull(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if _, err := db.Exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, price INTEGER NOT NULL);`); err != nil {
		t.Fatalf("seeding live table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO orders (price) VALUES (10), (-5);`); err != nil {
		t.Fatalf("seeding rows: %v", err)
	}

	// Clean plan: add an index and a new table; verify no mutation.
	res, err := exec.DryRunVerifySQLite(ctx, db, exec.SQLiteExecConfig{
		SchemaSQL: `
			CREATE TABLE orders (id INTEGER PRIMARY KEY, price INTEGER NOT NULL);
			CREATE INDEX idx_orders_price ON orders (price);
			CREATE TABLE shipments (id INTEGER PRIMARY KEY, order_id INTEGER REFERENCES orders (id));
		`,
	})
	if err != nil {
		t.Fatalf("DryRunVerifySQLite failed: %v", err)
	}
	if res.ExecutedSteps == 0 {
		t.Error("expected steps to execute in the sandbox")
	}
	var tblCount int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='shipments';`).Scan(&tblCount); err != nil {
		t.Fatalf("querying shipments: %v", err)
	}
	if tblCount != 0 {
		t.Error("shipments table must not persist after dry-run")
	}

	// Negative plan: NOT NULL column without default against existing rows
	// (NOT_NULL_NO_DEFAULT is a critical hazard; accept it so the dry-run
	// reaches execution and fails against live data).
	before, err := sqliteSnapshot(t, db)
	if err != nil {
		t.Fatalf("snapshotting before: %v", err)
	}
	_, err = exec.DryRunVerifySQLite(ctx, db, exec.SQLiteExecConfig{
		SchemaSQL: `
			CREATE TABLE orders (id INTEGER PRIMARY KEY, price INTEGER NOT NULL, status TEXT NOT NULL);
		`,
		AcceptHazards: []plan.HazardCode{plan.HazardNotNullNoDefault},
	})
	if err == nil {
		t.Fatal("expected dry-run to fail on NOT NULL violation against live rows")
	}
	if !strings.Contains(err.Error(), "dry-run verification failed at step") {
		t.Errorf("unexpected error: %v", err)
	}
	after, err := sqliteSnapshot(t, db)
	if err != nil {
		t.Fatalf("snapshotting after: %v", err)
	}
	if before != after {
		t.Error("sqlite schema changed after failed dry-run")
	}
}

func sqliteSnapshot(t *testing.T, db *sql.DB) (string, error) {
	t.Helper()
	rows, err := db.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		sb.WriteString(s)
		sb.WriteString("\n")
	}
	return sb.String(), rows.Err()
}
