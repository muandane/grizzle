//go:build integration

package grizzle_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/testutil"
)

func TestTimeouts_ConflictingHolder_RetrySucceeds(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("integration test failed: PostgreSQL/Docker unavailable at %s: %v (no silent skip allowed under integration tag)", connStr, err)
	}
	t.Logf("CI: running integration test %s against PostgreSQL at %s", t.Name(), connStr)

	schema := fmt.Sprintf("test_to_retry_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initialSQL := `CREATE TABLE accounts (id BIGINT PRIMARY KEY, balance NUMERIC);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// 1. Holder connection grabs an exclusive table lock
	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer holderConn.Close()

	holderTx, err := holderConn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("failed starting holder tx: %v", err)
	}
	defer func() { _ = holderTx.Rollback() }()

	_, err = holderTx.Exec(fmt.Sprintf(`LOCK TABLE %s.accounts IN ACCESS EXCLUSIVE MODE;`, schema))
	if err != nil {
		t.Fatalf("holder failed acquiring lock: %v", err)
	}

	// 2. Schedule holder tx release after 150ms
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = holderTx.Rollback()
	}()

	// 3. Migration attempts ALTER TABLE with a short lock timeout (50ms) and MaxRetries=5
	desiredSQL := `
		CREATE TABLE accounts (id BIGINT PRIMARY KEY, balance NUMERIC, currency TEXT);
	`

	start := time.Now()
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:          grizzle.DialectPostgres,
		TargetSchema:     schema,
		SchemaSQL:        desiredSQL,
		LockTimeout:      50 * time.Millisecond,
		StatementTimeout: 5 * time.Second,
		MaxRetries:       5,
	})
	if err != nil {
		t.Fatalf("migration failed despite retries: %v", err)
	}

	if time.Since(start) < 150*time.Millisecond {
		t.Errorf("migration should have retried until holder released (expected >= 150ms, took %v)", time.Since(start))
	}
}

func TestTimeouts_ConflictingHolder_ExhaustRetriesFails(t *testing.T) {
	connStr := testutil.PostgresDSN()
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open pg: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("integration test failed: PostgreSQL/Docker unavailable at %s: %v (no silent skip allowed under integration tag)", connStr, err)
	}
	t.Logf("CI: running integration test %s against PostgreSQL at %s", t.Name(), connStr)

	schema := fmt.Sprintf("test_to_fail_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema))
	}()

	initialSQL := `CREATE TABLE cards (id BIGINT PRIMARY KEY);`
	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    initialSQL,
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer holderConn.Close()

	holderTx, err := holderConn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("failed starting holder tx: %v", err)
	}
	defer func() { _ = holderTx.Rollback() }()

	_, err = holderTx.Exec(fmt.Sprintf(`LOCK TABLE %s.cards IN ACCESS EXCLUSIVE MODE;`, schema))
	if err != nil {
		t.Fatalf("holder failed acquiring lock: %v", err)
	}

	// Migration attempts ALTER TABLE with a 40ms lock timeout and MaxRetries=2; should fail
	desiredSQL := `
		CREATE TABLE cards (id BIGINT PRIMARY KEY, brand TEXT);
	`

	err = grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:          grizzle.DialectPostgres,
		TargetSchema:     schema,
		SchemaSQL:        desiredSQL,
		LockTimeout:      40 * time.Millisecond,
		StatementTimeout: 2 * time.Second,
		MaxRetries:       2,
	})
	if err == nil {
		t.Fatalf("expected migration to fail due to lock timeout, got nil")
	}

	if !exec.IsLockTimeout(err) && !strings.Contains(err.Error(), "55P03") && !strings.Contains(err.Error(), "lock timeout") {
		t.Errorf("expected lock timeout error (55P03), got: %v", err)
	}
}
