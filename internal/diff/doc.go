// Package diff performs pure relational comparison between live and desired database schemas.
//
// Dependency rule:
// diff is pure. It depends only on internal/schema, internal/scope, and internal/plan.
// It has zero dependencies on database/sql, context, or dialect rendering.
package diff
