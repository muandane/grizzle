// Package grizzle provides declarative, in-process database schema automigration for Go.
//
// Grizzle synchronizes PostgreSQL and SQLite database schemas directly from standard
// schema.sql DDL definitions on application startup, eliminating sequence file collisions,
// manual migration versioning, and external CLI/Docker runtime dependencies.
//
// # Core Concepts
//
//   - Declarative Schema: Write standard SQL DDL (schema.sql) as the single source of truth.
//   - In-Process Execution: Runs natively within your Go binary without needing external CLI binaries.
//   - Shadow Sandbox: Replays and validates desired schema state inside a temporary shadow schema or SQLite in-memory sandbox.
//   - Deterministic Diff: Pure schema comparison detecting missing tables, columns, indexes, constraints, and types.
//   - Safe Execution: Dedicated advisory locking, multi-node contention retry, hazard gating, and transactional execution.
//   - Zero-Downtime Operations: Safe non-transactional enum expansions (PostgreSQL ALTER TYPE ... ADD VALUE) and concurrent index generation.
//
// # Subpackages & Modules
//
//   - griztest: Testing primitives (MustSync, MustPlan, Reset) for effortless test database setup and teardown.
//   - otelgrizzle: Optional OpenTelemetry tracing integration (github.com/muandane/grizzle/otelgrizzle).
//
// # Basic Usage
//
//	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer db.Close()
//
//	err = grizzle.Sync(ctx, db, grizzle.Options{
//	    SchemaSQL: schemaSQL,
//	})
//	if err != nil {
//	    log.Fatalf("migration failed: %v", err)
//	}
package grizzle
