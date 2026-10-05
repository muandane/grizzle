package grizzle

import "github.com/yourorg/grizzle/internal/plan"

var (
	// ErrEmptySchema is returned when the provided SchemaSQL string is empty or contains only whitespace.
	ErrEmptySchema = plan.ErrEmptySchema

	// ErrDestructiveBlocked is returned when a destructive operation (such as DROP TABLE or DROP COLUMN)
	// is rejected by the configured drop safety policy.
	ErrDestructiveBlocked = plan.ErrDestructiveBlocked

	// ErrLockAcquisition is returned when acquiring the database advisory lock fails or times out.
	ErrLockAcquisition = plan.ErrLockAcquisition

	// ErrCompilationFailed is returned when the user's schema.sql fails to compile/execute inside the shadow schema.
	ErrCompilationFailed = plan.ErrCompilationFailed

	// ErrExecutionFailed is returned when applying the planned DDL migration steps to the target schema fails.
	ErrExecutionFailed = plan.ErrExecutionFailed

	// ErrInspectionFailed is returned when inspecting the schema metadata fails.
	ErrInspectionFailed = plan.ErrInspectionFailed

	// ErrUnsupportedDialect is returned when an unrecognized database driver or dialect is provided.
	ErrUnsupportedDialect = plan.ErrUnsupportedDialect
)

// DestructiveViolationError reports all destructive steps that were rejected by the active safety policy.
type DestructiveViolationError = plan.DestructiveViolationError
