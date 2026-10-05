package dialect

import (
	"context"
	"database/sql"

	"github.com/yourorg/grizzle/internal/plan"
	"github.com/yourorg/grizzle/internal/schema"
)

// DBTX specifies the common SQL executor interface satisfied by *sql.DB, *sql.Tx, and *sql.Conn.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Stmt represents a single SQL statement to be executed with its transactional requirement.
type Stmt struct {
	SQL   string
	NonTx bool
}

// Dialect specifies the database-specific behavior for introspection, DDL rendering, and locking.
type Dialect interface {
	Name() string
	Introspect(ctx context.Context, dbtx DBTX, targetSchema string) (*schema.Schema, error)
	Render(step plan.Step) ([]Stmt, error)
}
