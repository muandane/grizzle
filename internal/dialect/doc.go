// Package dialect defines the interface for database-specific introspection, DDL rendering,
// and transactional locking behaviors.
//
// Dependency rule:
// dialect is the only layer that knows SQL syntax and engine catalog details.
// It depends on internal/schema, internal/scope, internal/diff, and internal/plan.
package dialect
