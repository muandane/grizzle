//go:build integration

package grizzle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestPlanDiff_TerminateBackendMidCompile_NoShadowLeak kills the backend
// while PlanDiff compiles in its shadow transaction. The compile runs inside
// a single transaction, so a terminated backend must roll the shadow schema
// away server-side; no _grizzle_shadow% schema may survive.
func TestPlanDiff_TerminateBackendMidCompile_NoShadowLeak(t *testing.T) {
	db := testutil.TestDatabase(t)

	// The compile transaction runs the schema SQL verbatim; pg_sleep keeps
	// one backend visibly active long enough to terminate.
	schemaSQL := `
		CREATE TABLE leak_probe (id BIGINT PRIMARY KEY);
		SELECT pg_sleep(5);
		CREATE TABLE leak_probe2 (id BIGINT PRIMARY KEY);
	`

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	terminated := make(chan struct{})
	go func() {
		defer close(terminated)
		for {
			var pid int
			err := db.QueryRowContext(ctx, `
				SELECT pid FROM pg_stat_activity
				WHERE state = 'active'
				  AND query LIKE '%pg_sleep(5)%'
				  AND pid <> pg_backend_pid()
				LIMIT 1;
			`).Scan(&pid)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				time.Sleep(5 * time.Millisecond)
				continue
			}
			var success bool
			if err := db.QueryRowContext(ctx, "SELECT pg_terminate_backend($1);", pid).Scan(&success); err == nil && success {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	_, planErr := grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: "public",
		SchemaSQL:    schemaSQL,
	})
	<-terminated
	if planErr == nil {
		t.Fatalf("expected PlanDiff to fail after backend termination, got nil")
	}
	if !isTerminateError(planErr) && !strings.Contains(planErr.Error(), "canceling") && !strings.Contains(planErr.Error(), "terminated") {
		t.Logf("unexpected error shape (still acceptable if no shadow leaked): %v", planErr)
	}

	// Server-side rollback must leave no shadow schemas behind. Poll briefly:
	// the terminated backend's rollback is asynchronous.
	deadline := time.Now().Add(3 * time.Second)
	var leaked int
	for {
		if err := db.QueryRow(`SELECT count(*) FROM pg_namespace WHERE nspname LIKE '\_grizzle\_shadow%';`).Scan(&leaked); err != nil {
			t.Fatalf("querying shadow schemas: %v", err)
		}
		if leaked == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("found %d leftover _grizzle_shadow%% schema(s) after terminated compile", leaked)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func isTerminateError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "57P01" || pgErr.Code == "57014")
}
