// Package plan models migration steps, execution plans, safety policies, and hazard detection.
//
// Dependency rule:
// plan is pure. It depends only on internal/schema and standard library packages (fmt, io, strings, etc.).
// It has zero dependencies on database/sql, context, or network I/O.
package plan
