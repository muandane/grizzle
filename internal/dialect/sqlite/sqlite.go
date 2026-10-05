package sqlite

import (
	"context"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// SQLite implements the dialect.Dialect interface for SQLite.
type SQLite struct{}

func New() *SQLite {
	return &SQLite{}
}

func (s *SQLite) Name() string {
	return "sqlite"
}

func (s *SQLite) Introspect(ctx context.Context, dbtx dialect.DBTX, targetSchema string) (*schema.Schema, error) {
	return Inspect(ctx, dbtx)
}

func (s *SQLite) Render(step plan.Step) ([]dialect.Stmt, error) {
	return Render(step)
}
