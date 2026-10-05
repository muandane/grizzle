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
	shadowDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("sqlite: failed to open in-memory shadow db: %w", err)
	}
	defer func() { _ = shadowDB.Close() }()

	if _, err := shadowDB.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("sqlite: shadow compilation failed: %w", err)
	}

	return Inspect(ctx, shadowDB)
}
