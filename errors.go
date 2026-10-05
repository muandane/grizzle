package grizzle

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrEmptySchema is returned when the provided SchemaSQL string is empty or contains only whitespace.
	ErrEmptySchema = errors.New("grizzle: schema SQL cannot be empty")

	// ErrDestructiveBlocked is returned when a destructive operation (such as DROP TABLE or DROP COLUMN)
	// is rejected by the configured drop safety policy.
	ErrDestructiveBlocked = errors.New("grizzle: destructive change rejected by policy (drop disallowed)")

	// ErrLockAcquisition is returned when acquiring the PostgreSQL advisory lock fails or times out.
	ErrLockAcquisition = errors.New("grizzle: failed to acquire migration advisory lock")

	// ErrCompilationFailed is returned when the user's schema.sql fails to compile/execute inside the shadow schema.
	ErrCompilationFailed = errors.New("grizzle: failed to execute schema.sql in shadow schema")

	// ErrExecutionFailed is returned when applying the planned DDL migration steps to the target schema fails.
	ErrExecutionFailed = errors.New("grizzle: failed to apply migration DDL to live schema")
)

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

func (e *DestructiveViolationError) Is(target error) bool {
	return target == ErrDestructiveBlocked
}
