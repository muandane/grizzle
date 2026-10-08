package plan

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrEmptySchema is returned when the provided SchemaSQL string is empty or contains only whitespace.
	ErrEmptySchema = errors.New("grizzle: schema SQL cannot be empty")

	// ErrDestructiveBlocked is returned when a destructive operation is rejected by the drop safety policy.
	ErrDestructiveBlocked = errors.New("grizzle: destructive change rejected by policy (drop disallowed)")

	// ErrLockAcquisition is returned when acquiring an advisory lock fails or times out.
	ErrLockAcquisition = errors.New("grizzle: failed to acquire migration advisory lock")

	// ErrCompilationFailed is returned when the user's schema.sql fails to compile in the shadow schema.
	ErrCompilationFailed = errors.New("grizzle: failed to execute schema.sql in shadow schema")

	// ErrExecutionFailed is returned when applying the planned DDL migration steps fails.
	ErrExecutionFailed = errors.New("grizzle: failed to apply migration DDL to live schema")

	// ErrInspectionFailed is returned when inspecting the schema metadata fails.
	ErrInspectionFailed = errors.New("grizzle: inspecting schema failed")

	// ErrUnsupportedDialect is returned when an unrecognized database driver or dialect is provided.
	ErrUnsupportedDialect = errors.New("grizzle: unsupported database dialect")

	// ErrHazardBlocked is returned when a migration plan contains critical hazards not explicitly accepted in AcceptHazards.
	ErrHazardBlocked = errors.New("grizzle: critical hazard rejected (unaccepted hazard)")

	// ErrPlanDrift is returned when the approved plan hash differs from the recomputed plan hash after lock acquisition.
	ErrPlanDrift = errors.New("grizzle: plan drift detected (hash mismatch)")

	// ErrDrift is returned by Check when the live database schema differs from the desired schema.
	ErrDrift = errors.New("grizzle: database schema drift detected")

	// ErrLockTimeout is returned when an advisory lock cannot be acquired within the configured lock timeout.
	ErrLockTimeout = errors.New("grizzle: lock acquisition timeout")

	// ErrInvalidOptions is returned when provided Options fail validation.
	ErrInvalidOptions = errors.New("grizzle: invalid options")

	// ErrStrictScope is returned when StrictScope is enabled but no IncludeTables are specified.
	ErrStrictScope = errors.New("grizzle: strict scope violation (IncludeTables required)")

	// ErrPartitionConversion is returned when attempting to alter a regular table into a partitioned table in-place.
	ErrPartitionConversion = errors.New("grizzle: in-place table partitioning conversion rejected")

	// ErrUnsupportedMultiSchema is returned when CompileSchema is asked to
	// compile multiple PostgreSQL schemas in one shadow (Sync handles PG
	// multi-schema; SQLite multi-schema uses ATTACH).
	ErrUnsupportedMultiSchema = errors.New("grizzle: multi-schema CompileSchema is unsupported for PostgreSQL")

	// ErrPartitionKeyNotInUnique is returned when a primary key or unique constraint on a partitioned table does not include all partition key columns.
	ErrPartitionKeyNotInUnique = errors.New("grizzle: primary key or unique constraint on partitioned table must include all partition key columns")

	// ErrAfterSyncFailed is returned when the AfterSync hook fails after the
	// migration steps have already committed.
	ErrAfterSyncFailed = errors.New("grizzle: after_sync hook failed (migration already committed)")

	// ErrSeedFailed is returned when seed SQL execution fails; the seed
	// transaction is rolled back and the schema is left intact.
	ErrSeedFailed = errors.New("grizzle: seed execution failed")

	// ErrHistoryRecord is returned when the migration itself succeeded but
	// the grizzle_history record could not be written. It is non-fatal in
	// the sense that the schema changes are committed; callers may treat it
	// as advisory via errors.Is and re-record or alert.
	ErrHistoryRecord = errors.New("grizzle: migration succeeded but history record was not written")
)

// HazardError reports all critical hazards that were not explicitly accepted.
type HazardError struct {
	Hazards []Hazard
}

func (e *HazardError) Error() string {
	var codes []string
	for _, h := range e.Hazards {
		codes = append(codes, string(h.Code))
	}
	return fmt.Sprintf("grizzle: blocked by unaccepted critical hazard(s) [%s]", strings.Join(codes, ", "))
}

// Is reports whether target matches ErrHazardBlocked.
func (e *HazardError) Is(target error) bool {
	return target == ErrHazardBlocked
}

func (e *HazardError) Unwrap() error {
	return ErrHazardBlocked
}

// DestructiveViolationError reports all destructive steps that were rejected by the active safety policy.
type DestructiveViolationError struct {
	Violations []Step
}

func (e *DestructiveViolationError) Error() string {
	if len(e.Violations) == 1 {
		return fmt.Sprintf("grizzle: destructive change [%s on %s] rejected by safety policy", e.Violations[0].Type, e.Violations[0].Table)
	}
	var details []string
	for _, v := range e.Violations {
		details = append(details, fmt.Sprintf("%s on %s", v.Type, v.Table))
	}
	return fmt.Sprintf("grizzle: %d destructive changes rejected by safety policy: [%s]", len(e.Violations), strings.Join(details, ", "))
}

// Is reports whether target matches ErrDestructiveBlocked.
func (e *DestructiveViolationError) Is(target error) bool {
	return target == ErrDestructiveBlocked
}

// DriftError reports unapplied differences between the live schema and the desired schema.
type DriftError struct {
	Plan *Plan
}

func (e *DriftError) Error() string {
	if e.Plan == nil || len(e.Plan.Steps) == 0 {
		return "grizzle: database schema drift detected"
	}
	return fmt.Sprintf("grizzle: schema drift detected: %d change(s) difference between live and desired schemas", len(e.Plan.Steps))
}

// Is reports whether target matches ErrDrift.
func (e *DriftError) Is(target error) bool {
	return target == ErrDrift
}

func (e *DriftError) Unwrap() error {
	return ErrDrift
}
