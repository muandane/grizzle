package grizzle_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	_ "github.com/jackc/pgx/v5/stdlib"
	"modernc.org/sqlite"
	_ "modernc.org/sqlite"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/testutil"
)

// wrappedDriver simulates instrumentation wrappers (otelsql, sqlx) that embed
// another driver in an unexported field; the concrete type name carries no
// vendor signal, so detection must return DialectAuto for it.
type wrappedDriver struct {
	driver.Driver
}

func TestDetectDialectFromDriver(t *testing.T) {
	cases := []struct {
		name string
		drv  driver.Driver
		want grizzle.Dialect
	}{
		{"pgx stdlib", stdlib.GetDefaultDriver(), grizzle.DialectPostgres},
		{"modernc sqlite", &sqlite.Driver{}, grizzle.DialectSQLite},
		{"unknown wrapper", wrappedDriver{stdlib.GetDefaultDriver()}, grizzle.DialectAuto},
		{"nil", nil, grizzle.DialectAuto},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := grizzle.DetectDialectFromDriver(tc.drv)
			if got != tc.want {
				t.Errorf("DetectDialectFromDriver(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestDetectDialect_WrappedDriverFallsBackToProbe verifies the end-to-end
// fallback: a wrapper driver is unrecognized, so detectDialect must recover by
// probing the live database instead of failing. Requires Postgres; skips when
// unreachable.
func TestDetectDialect_WrappedDriverFallsBackToProbe(t *testing.T) {
	registerName := "grizzle-test-wrapped-driver"
	if !isDriverRegistered(registerName) {
		sql.Register(registerName, wrappedDriver{stdlib.GetDefaultDriver()})
	}

	db, err := sql.Open(registerName, testutil.PostgresDSN())
	if err != nil {
		t.Fatalf("opening wrapped driver: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Ping(); err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}

	got, err := grizzle.DetectDialectForTest(context.Background(), db)
	if err != nil {
		t.Fatalf("DetectDialect with wrapped driver: %v", err)
	}
	if got != grizzle.DialectPostgres {
		t.Errorf("DetectDialect = %v, want %v (probe fallback must identify postgres)", got, grizzle.DialectPostgres)
	}
}

func isDriverRegistered(name string) bool {
	return slices.Contains(sql.Drivers(), name)
}
