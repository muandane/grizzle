package sqlite

import (
	"context"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// SQLite implements the dialect.Dialect interface for SQLite.
type SQLite struct{}

// New returns a new SQLite dialect instance.
func New() *SQLite {
	return &SQLite{}
}

// Name returns the dialect identifier "sqlite".
func (s *SQLite) Name() string {
	return "sqlite"
}

// Introspect inspects the SQLite database and returns the schema IR.
func (s *SQLite) Introspect(ctx context.Context, dbtx dialect.DBTX, targetSchema string) (*schema.Schema, error) {
	return Inspect(ctx, dbtx)
}

// Render translates a migration step into SQLite SQL statements.
func (s *SQLite) Render(step plan.Step) ([]dialect.Stmt, error) {
	return Render(step)
}

