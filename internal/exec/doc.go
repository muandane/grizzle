// Package exec coordinates statement execution, transactional locking, and safety policy enforcement.
//
// Dependency rule:
// exec is the execution layer. It coordinates dialects, plans, schemas, and scopes.
// It has zero knowledge of the public root API.
package exec
