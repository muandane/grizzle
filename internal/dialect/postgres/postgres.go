package postgres

import (
	"context"

	"github.com/yourorg/grizzle/internal/dialect"
	"github.com/yourorg/grizzle/internal/plan"
	"github.com/yourorg/grizzle/internal/schema"
)

// Postgres implements the dialect.Dialect interface for PostgreSQL.
type Postgres struct{}

func New() *Postgres {
	return &Postgres{}
}

func (p *Postgres) Name() string {
	return "postgres"
}

func (p *Postgres) Introspect(ctx context.Context, dbtx dialect.DBTX, targetSchema string) (*schema.Schema, error) {
	return Inspect(ctx, dbtx, targetSchema)
}

func (p *Postgres) Render(step plan.Step) ([]dialect.Stmt, error) {
	return Render(step)
}
