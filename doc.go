// Package grizzle provides declarative, in-process database schema automigration for Go.
//
// Grizzle synchronizes PostgreSQL and SQLite database schemas directly from standard
// schema.sql DDL files on application startup, eliminating sequence file collisions,
// manual migration versioning, and external CLI/Docker runtime dependencies.
package grizzle
