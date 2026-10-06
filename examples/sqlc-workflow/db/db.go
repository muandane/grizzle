// Package db provides type-safe database queries.
package db

import (
	"context"
	"database/sql"
)

// DB represents a database connection or transaction.
type DB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	PrepareContext(context.Context, string) (*sql.Stmt, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// New returns a new Queries instance bound to db.
func New(db DB) *Queries {
	return &Queries{db: db}
}

// Queries provides query execution methods.
type Queries struct {
	db DB
}

// WithTx returns a new Queries instance scoped to a transaction.
func (q *Queries) WithTx(tx *sql.Tx) *Queries {
	return &Queries{
		db: tx,
	}
}
