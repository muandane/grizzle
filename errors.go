package grizzle

import "errors"

var (
	// ErrEmptySchema is returned when the provided SchemaSQL string is empty or contains only whitespace.
	ErrEmptySchema = errors.New("grizzle: schema SQL cannot be empty")

	// ErrDestructiveBlocked is returned when a destructive operation (such as DROP TABLE or DROP COLUMN)
	// is detected while Options.AllowDrop is set to false.
	ErrDestructiveBlocked = errors.New("grizzle: destructive change rejected by policy (AllowDrop is false)")

	// ErrLockAcquisition is returned when acquiring the PostgreSQL advisory lock fails or times out.
	ErrLockAcquisition = errors.New("grizzle: failed to acquire migration advisory lock")

	// ErrCompilationFailed is returned when the user's schema.sql fails to compile/execute inside the shadow schema.
	ErrCompilationFailed = errors.New("grizzle: failed to execute schema.sql in shadow schema")

	// ErrExecutionFailed is returned when applying the planned DDL migration steps to the target schema fails.
	ErrExecutionFailed = errors.New("grizzle: failed to apply migration DDL to live schema")
)
