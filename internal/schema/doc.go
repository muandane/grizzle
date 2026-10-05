// Package schema defines the pure, engine-agnostic relational schema representation
// (Table, Column, Index, Constraint, Schema) and normalization logic.
//
// Dependency rule:
// schema is the foundation layer. It must remain completely pure and has zero
// dependencies on database/sql, context, I/O, or globals.
package schema
