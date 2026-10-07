package grizzle

import "github.com/muandane/grizzle/internal/plan"

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

	// ErrHazardBlocked is returned when a migration plan contains critical hazards not explicitly accepted in AcceptHazards.
	ErrHazardBlocked = plan.ErrHazardBlocked

	// ErrPlanDrift is returned when the approved plan hash differs from the recomputed plan hash after lock acquisition.
	ErrPlanDrift = plan.ErrPlanDrift

	// ErrDrift is returned by Check when the live database schema differs from the desired schema.
	ErrDrift = plan.ErrDrift

	// ErrLockTimeout is returned when an advisory lock cannot be acquired within the configured lock timeout.
	ErrLockTimeout = plan.ErrLockTimeout

	// ErrInvalidOptions is returned when provided Options fail validation.
	ErrInvalidOptions = plan.ErrInvalidOptions

	// ErrStrictScope is returned when StrictScope is enabled but no IncludeTables are specified.
	ErrStrictScope = plan.ErrStrictScope

	// ErrPartitionConversion is returned when attempting to convert a regular table to a partitioned table or vice versa in-place.
	ErrPartitionConversion = plan.ErrPartitionConversion

	// ErrUnsupportedMultiSchema is returned when multiple schemas are configured on SQLite.
	ErrUnsupportedMultiSchema = plan.ErrUnsupportedMultiSchema

	// ErrPartitionKeyNotInUnique is returned when a primary key or unique constraint on a partitioned table does not include all partition key columns.
	ErrPartitionKeyNotInUnique = plan.ErrPartitionKeyNotInUnique
)

// HazardError reports all critical hazards that were not explicitly accepted.
type HazardError = plan.HazardError

// DestructiveViolationError reports all destructive steps that were rejected by the active safety policy.
type DestructiveViolationError = plan.DestructiveViolationError

// DriftError reports unapplied differences between the live schema and the desired schema.
type DriftError = plan.DriftError
