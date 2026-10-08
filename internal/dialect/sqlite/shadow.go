package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/muandane/grizzle/internal/schema"
	_ "modernc.org/sqlite"
)

// CompileInShadow executes schemaSQL in an isolated in-memory SQLite database and returns the introspected schema.
func CompileInShadow(ctx context.Context, schemaSQL string) (*schema.Schema, error) {
	m, err := CompileInShadowSchemas(ctx, schemaSQL, []string{"main"})
	if err != nil {
		return nil, err
	}
	return m["main"], nil
}

// CompileInShadowSchemas compiles schemaSQL against a private :memory: connection
// with ATTACH ':memory:' AS name for every non-main target schema. Live filesystem
// paths are never opened — shadow attaches are empty in-memory databases so desired
// DDL compiles without mutating production files.
func CompileInShadowSchemas(ctx context.Context, schemaSQL string, schemas []string) (map[string]*schema.Schema, error) {
	if len(schemas) == 0 {
		schemas = []string{"main"}
	}

	shadowDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("sqlite: failed to open in-memory shadow db: %w", err)
	}
	defer func() { _ = shadowDB.Close() }()

	// Pin a connection so ATTACH state is visible to subsequent Exec/Inspect.
	conn, err := shadowDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: failed to acquire shadow connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := AttachMemorySchemas(ctx, conn, schemas); err != nil {
		return nil, err
	}

	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("sqlite: shadow compilation failed: %w", err)
	}

	return InspectSchemas(ctx, conn, schemas)
}
