// Package scope handles table filtering and unmanaged-table boundaries (PostGIS,
// background queues, and user-specified include/exclude patterns).
//
// Dependency rule:
// scope is pure. It has zero dependencies on database/sql, context, I/O, or globals.
package scope
