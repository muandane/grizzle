package postgres

import (
	"context"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// Postgres implements the dialect.Dialect interface for PostgreSQL.
type Postgres struct{}

// New instantiates a new PostgreSQL dialect handler.
func New() *Postgres {
	return &Postgres{}
}

// Name returns the canonical name of the dialect.
func (p *Postgres) Name() string {
	return "postgres"
}

// Introspect extracts the relational schema from the live PostgreSQL database.
func (p *Postgres) Introspect(ctx context.Context, dbtx dialect.DBTX, targetSchema string) (*schema.Schema, error) {
	return Inspect(ctx, dbtx, targetSchema)
}

// Render compiles an individual migration step into execution statements.
func (p *Postgres) Render(step plan.Step) ([]dialect.Stmt, error) {
	return Render(step)
}
