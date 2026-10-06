package otelgrizzle_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/otelgrizzle"
	"go.opentelemetry.io/otel/trace/noop"
	_ "modernc.org/sqlite"
)

func TestOTELGrizzle_Integration(t *testing.T) {
	// Use noop OpenTelemetry tracer to verify type compatibility and full sync execution
	tp := noop.NewTracerProvider()
	tracer := tp.Tracer("grizzle-test")

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `
CREATE TABLE products (
    id INTEGER PRIMARY KEY,
    title TEXT NOT NULL,
    price INTEGER NOT NULL DEFAULT 0
);`

	opts := grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schema,
	}
	otelgrizzle.WithTracer(tracer)(&opts)

	err = grizzle.Sync(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("grizzle sync with otel tracer failed: %v", err)
	}

	// Verify table created
	var count int
	if err := db.QueryRow("SELECT count(*) FROM products;").Scan(&count); err != nil {
		t.Fatalf("querying table: %v", err)
	}
}
