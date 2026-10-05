package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

func TestParity_Renames_AmbiguousAndExplicit(t *testing.T) {
	ctx := context.Background()

	runTest := func(t *testing.T, dialect grizzle.Dialect, db *sql.DB, targetSchema string) {
		trueVal := true
		opts := func(sqlStr string, renames map[string]string) grizzle.Options {
			return grizzle.Options{
				Dialect:         dialect,
				TargetSchema:    targetSchema,
				SchemaSQL:       sqlStr,
				Renames:         renames,
				AllowDropColumn: &trueVal,
			}
		}

		// Initial: user_name TEXT
		initSQL := `CREATE TABLE accounts (id INTEGER PRIMARY KEY, user_name TEXT);`
		if dialect == grizzle.DialectPostgres {
			initSQL = `CREATE TABLE accounts (id BIGINT PRIMARY KEY, user_name TEXT);`
		}
		if err := grizzle.Sync(ctx, db, opts(initSQL, nil)); err != nil {
			t.Fatalf("[%s] initial sync failed: %v", dialect, err)
		}

		// 1. Ambiguous rename: user_name -> handle (both TEXT, no rename mapping)
		desiredSQL := `CREATE TABLE accounts (id INTEGER PRIMARY KEY, handle TEXT);`
		if dialect == grizzle.DialectPostgres {
			desiredSQL = `CREATE TABLE accounts (id BIGINT PRIMARY KEY, handle TEXT);`
		}

		pAmbiguous, err := grizzle.PlanDiff(ctx, db, opts(desiredSQL, nil))
		if err != nil {
			t.Fatalf("[%s] PlanDiff ambiguous failed: %v", dialect, err)
		}

		var hasAmbiguousHazard bool
		for _, h := range pAmbiguous.Hazards() {
			if h.Code == grizzle.HazardRenameAmbiguous {
				hasAmbiguousHazard = true
				if h.Level != grizzle.HazardLevelCritical {
					t.Errorf("[%s] expected HazardRenameAmbiguous level CRITICAL, got %s", dialect, h.Level)
				}
			}
		}
		if !hasAmbiguousHazard {
			t.Fatalf("[%s] expected HazardRenameAmbiguous for unmapped candidate rename", dialect)
		}

		// Apply without accepting hazard must fail with ErrHazardBlocked
		err = grizzle.Apply(ctx, db, pAmbiguous, grizzle.ApplyOpts{})
		if !errors.Is(err, grizzle.ErrHazardBlocked) {
			t.Fatalf("[%s] expected ErrHazardBlocked on ambiguous rename, got: %v", dialect, err)
		}

		// 2. Explicit rename mapping: accounts.user_name -> handle
		pExplicit, err := grizzle.PlanDiff(ctx, db, opts(desiredSQL, map[string]string{
			"accounts.user_name": "handle",
		}))
		if err != nil {
			t.Fatalf("[%s] PlanDiff explicit failed: %v", dialect, err)
		}

		for _, h := range pExplicit.Hazards() {
			if h.Code == grizzle.HazardRenameAmbiguous {
				t.Fatalf("[%s] explicit rename mapping must NOT emit HazardRenameAmbiguous", dialect)
			}
		}

		// Apply explicit rename
		err = grizzle.Apply(ctx, db, pExplicit, grizzle.ApplyOpts{})
		if err != nil {
			t.Fatalf("[%s] explicit rename apply failed: %v", dialect, err)
		}
	}

	t.Run("SQLite", func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("failed opening sqlite: %v", err)
		}
		defer func() { _ = db.Close() }()
		runTest(t, grizzle.DialectSQLite, db, "main")
	})

	t.Run("Postgres", func(t *testing.T) {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			dsn = os.Getenv("POSTGRES_DSN")
		}
		if dsn == "" {
			dsn = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("failed opening postgres: %v", err)
		}
		defer func() { _ = db.Close() }()
		if err := db.Ping(); err != nil {
			t.Skipf("skipping postgres parity test, not reachable: %v", err)
		}

		schema := fmt.Sprintf("test_parity_rename_%d", time.Now().UnixNano())
		_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
		if err != nil {
			t.Fatalf("failed creating schema: %v", err)
		}
		defer func() {
			_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
		}()

		runTest(t, grizzle.DialectPostgres, db, schema)
	})
}

func TestParity_Hazards_TypeNarrowing(t *testing.T) {
	ctx := context.Background()

	runTest := func(t *testing.T, dialect grizzle.Dialect, db *sql.DB, targetSchema string) {
		trueVal := true
		opts := func(sqlStr string) grizzle.Options {
			return grizzle.Options{
				Dialect:         dialect,
				TargetSchema:    targetSchema,
				SchemaSQL:       sqlStr,
				AllowDropColumn: &trueVal,
			}
		}

		// Initial: BIGINT
		initSQL := `CREATE TABLE metrics (id INTEGER PRIMARY KEY, count BIGINT);`
		if dialect == grizzle.DialectPostgres {
			initSQL = `CREATE TABLE metrics (id BIGINT PRIMARY KEY, count BIGINT);`
		}
		if err := grizzle.Sync(ctx, db, opts(initSQL)); err != nil {
			t.Fatalf("[%s] initial sync failed: %v", dialect, err)
		}

		// Desired: INTEGER (narrowed from BIGINT)
		desiredSQL := `CREATE TABLE metrics (id INTEGER PRIMARY KEY, count INTEGER);`
		if dialect == grizzle.DialectPostgres {
			desiredSQL = `CREATE TABLE metrics (id BIGINT PRIMARY KEY, count INTEGER);`
		}

		p, err := grizzle.PlanDiff(ctx, db, opts(desiredSQL))
		if err != nil {
			t.Fatalf("[%s] PlanDiff failed: %v", dialect, err)
		}

		var hasTypeNarrow bool
		for _, h := range p.Hazards() {
			if h.Code == grizzle.HazardTypeNarrow {
				hasTypeNarrow = true
				if h.Level != grizzle.HazardLevelCritical {
					t.Errorf("[%s] expected HazardTypeNarrow to be CRITICAL, got %s", dialect, h.Level)
				}
			}
		}
		if !hasTypeNarrow {
			t.Fatalf("[%s] expected HazardTypeNarrow when changing BIGINT to INTEGER, got hazards: %+v", dialect, p.Hazards())
		}

		// Apply without accepting must fail
		err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{})
		if !errors.Is(err, grizzle.ErrHazardBlocked) {
			t.Fatalf("[%s] expected ErrHazardBlocked without AcceptHazards, got: %v", dialect, err)
		}

		// Apply with AcceptHazards: [HazardTypeNarrow] must succeed
		err = grizzle.Apply(ctx, db, p, grizzle.ApplyOpts{
			AcceptHazards: []grizzle.HazardCode{grizzle.HazardTypeNarrow},
		})
		if err != nil {
			t.Fatalf("[%s] expected apply to succeed with accepted HazardTypeNarrow, got: %v", dialect, err)
		}
	}

	t.Run("SQLite", func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("failed opening sqlite: %v", err)
		}
		defer func() { _ = db.Close() }()
		runTest(t, grizzle.DialectSQLite, db, "main")
	})

	t.Run("Postgres", func(t *testing.T) {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			dsn = os.Getenv("POSTGRES_DSN")
		}
		if dsn == "" {
			dsn = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("failed opening postgres: %v", err)
		}
		defer func() { _ = db.Close() }()
		if err := db.Ping(); err != nil {
			t.Skipf("skipping postgres parity test, not reachable: %v", err)
		}

		schema := fmt.Sprintf("test_parity_typenarrow_%d", time.Now().UnixNano())
		_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
		if err != nil {
			t.Fatalf("failed creating schema: %v", err)
		}
		defer func() {
			_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
		}()

		runTest(t, grizzle.DialectPostgres, db, schema)
	})
}

func TestParity_StrictScope(t *testing.T) {
	ctx := context.Background()

	runTest := func(t *testing.T, dialect grizzle.Dialect, db *sql.DB, targetSchema string) {
		// 1. StrictScope: true without IncludeTables must immediately fail with ErrStrictScope
		err := grizzle.Sync(ctx, db, grizzle.Options{
			Dialect:      dialect,
			TargetSchema: targetSchema,
			SchemaSQL:    `CREATE TABLE managed (id INT PRIMARY KEY);`,
			StrictScope:  true,
		})
		if !errors.Is(err, grizzle.ErrStrictScope) {
			t.Fatalf("[%s] expected ErrStrictScope when IncludeTables is empty, got: %v", dialect, err)
		}

		// Setup: Create two tables in DB directly (managed and external_worker)
		createSQL := fmt.Sprintf(`CREATE TABLE %s (id INT PRIMARY KEY);`, "managed")
		if dialect == grizzle.DialectPostgres {
			createSQL = fmt.Sprintf(`CREATE TABLE %s.managed (id INT PRIMARY KEY);`, targetSchema)
		}
		if _, err := db.Exec(createSQL); err != nil {
			t.Fatalf("[%s] failed creating managed table: %v", dialect, err)
		}

		extSQL := fmt.Sprintf(`CREATE TABLE %s (id INT PRIMARY KEY);`, "external_worker")
		if dialect == grizzle.DialectPostgres {
			extSQL = fmt.Sprintf(`CREATE TABLE %s.external_worker (id INT PRIMARY KEY);`, targetSchema)
		}
		if _, err := db.Exec(extSQL); err != nil {
			t.Fatalf("[%s] failed creating external_worker table: %v", dialect, err)
		}

		// 2. Sync with StrictScope: true and IncludeTables: ["managed"]
		// Even with AllowDrop: true, external_worker must NEVER be dropped because it is out of scope!
		trueVal := true
		err = grizzle.Sync(ctx, db, grizzle.Options{
			Dialect:        dialect,
			TargetSchema:   targetSchema,
			SchemaSQL:      `CREATE TABLE managed (id INT PRIMARY KEY, name TEXT);`,
			StrictScope:    true,
			IncludeTables:  []string{"managed"},
			AllowDropTable: &trueVal,
		})
		if err != nil {
			t.Fatalf("[%s] sync with strict scope failed: %v", dialect, err)
		}

		// Verify external_worker still exists!
		checkSQL := `SELECT COUNT(*) FROM external_worker;`
		if dialect == grizzle.DialectPostgres {
			checkSQL = fmt.Sprintf(`SELECT COUNT(*) FROM %s.external_worker;`, targetSchema)
		}
		var count int
		if err := db.QueryRow(checkSQL).Scan(&count); err != nil {
			t.Fatalf("[%s] external_worker table was deleted or inaccessible, expected it to be untouched: %v", dialect, err)
		}

		// 3. ExcludeTables wildcard: "asynq_*"
		asynqSQL := fmt.Sprintf(`CREATE TABLE %s (id INT PRIMARY KEY);`, "asynq_tasks")
		if dialect == grizzle.DialectPostgres {
			asynqSQL = fmt.Sprintf(`CREATE TABLE %s.asynq_tasks (id INT PRIMARY KEY);`, targetSchema)
		}
		if _, err := db.Exec(asynqSQL); err != nil {
			t.Fatalf("[%s] failed creating asynq_tasks: %v", dialect, err)
		}

		err = grizzle.Sync(ctx, db, grizzle.Options{
			Dialect:        dialect,
			TargetSchema:   targetSchema,
			SchemaSQL:      `CREATE TABLE managed (id INT PRIMARY KEY, name TEXT);`,
			ExcludeTables:  []string{"asynq_*", "external_*"},
			AllowDropTable: &trueVal,
		})
		if err != nil {
			t.Fatalf("[%s] sync with ExcludeTables failed: %v", dialect, err)
		}

		// Verify asynq_tasks was NOT dropped
		asynqCheck := `SELECT COUNT(*) FROM asynq_tasks;`
		if dialect == grizzle.DialectPostgres {
			asynqCheck = fmt.Sprintf(`SELECT COUNT(*) FROM %s.asynq_tasks;`, targetSchema)
		}
		if err := db.QueryRow(asynqCheck).Scan(&count); err != nil {
			t.Fatalf("[%s] asynq_tasks table was dropped despite ExcludeTables wildcard: %v", dialect, err)
		}
	}

	t.Run("SQLite", func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("failed opening sqlite: %v", err)
		}
		defer func() { _ = db.Close() }()
		runTest(t, grizzle.DialectSQLite, db, "main")
	})

	t.Run("Postgres", func(t *testing.T) {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			dsn = os.Getenv("POSTGRES_DSN")
		}
		if dsn == "" {
			dsn = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("failed opening postgres: %v", err)
		}
		defer func() { _ = db.Close() }()
		if err := db.Ping(); err != nil {
			t.Skipf("skipping postgres parity test, not reachable: %v", err)
		}

		schema := fmt.Sprintf("test_parity_scope_%d", time.Now().UnixNano())
		_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
		if err != nil {
			t.Fatalf("failed creating schema: %v", err)
		}
		defer func() {
			_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
		}()

		runTest(t, grizzle.DialectPostgres, db, schema)
	})
}
