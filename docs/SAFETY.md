# Database migration safety in production

Grizzle applies declarative database migrations directly inside the application process on boot. This document evaluates how Grizzle, Drizzle ORM, Atlas, Ent, and TypeORM handle schema updates in production environments, and documents the safety invariants Grizzle enforces in code.

## Production safety comparison

Production database migrations face four core failure modes: concurrent replica execution, silent data loss from dropped columns or tables, runtime errors from incompatible column modifications, and accidental deletion of unmanaged tables (such as PostGIS metadata or background worker queues).

The following table compares how each tool handles these failure modes:

| Capability | TypeORM (`synchronize`) | Drizzle (`drizzle-kit push`) | Atlas CLI | Ent (`entgo.io`) | Grizzle |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Runtime mode** | In-process | External Node.js CLI | External Go binary / Docker | In-process | In-process |
| **Schema definition** | TypeScript entities | TypeScript schema files | HCL or SQL | Go structs | Standard `schema.sql` |
| **Distributed locking** | None (races on multi-pod boot) | None (single-process CLI) | Transactional advisory locks | Custom / driver-dependent | `pg_advisory_xact_lock` |
| **Pre-flight shadow validation** | None | None (executes on target) | Dev database / Docker container | In-memory parser | Isolated shadow schema |
| **Default drop behavior** | Drops unmapped tables/columns | Prompts in TTY; drops with `--force` | Configurable via flags | Drops disabled (`WithDropColumn(false)`) | Drops blocked (`AllowDrop: false`) |
| **Granular drop controls** | None | None | Per-resource policies in HCL | Table and column flags | Separate table, column, index, FK flags |
| **Unmanaged table protection** | None (drops third-party tables) | None | `exclude` pattern list | `WithTables(...)` whitelist | `ExcludeTables`, `IncludeTables`, extension filters |
| **Pre-execution hazard analysis** | None | None | Analyzer rules (some behind paywall) | None | Built-in `Plan.Hazards()` |
| **Execution atomicity** | Partial (statement-by-statement) | Transactional where supported | Transactional | Transactional | Single transaction with automatic rollback |

## Detailed analysis of existing tools

### TypeORM (`synchronize: true`)

TypeORM includes a `synchronize` flag intended for rapid local development. In production, this flag causes immediate operational failures:

1. **No distributed locking.** When three Kubernetes pods start simultaneously, all three introspect the database at the same instant, compute identical or overlapping schema changes, and attempt concurrent `ALTER TABLE` statements. This causes PostgreSQL deadlocks (`deadlock detected`), lock acquisition timeouts, and failed application boots.
2. **Silent data loss.** If a property is renamed or omitted from an entity class, TypeORM executes `ALTER TABLE ... DROP COLUMN` or drops the entire table on startup without user approval.
3. **No shadow validation.** TypeORM applies generated SQL statements directly to the target database. If a statement fails midway (for instance, a NOT NULL constraint on a column with existing NULL values), the database is left in a partially altered state.
4. **Third-party table corruption.** TypeORM treats any table not mapped to an active entity as obsolete and attempts to drop it. This deletes tables managed by background job processors (`asynq`, `pgboss`, `temporal`) and database extensions (`spatial_ref_sys`).

### Drizzle ORM (`drizzle-kit push`)

Drizzle ORM provides `drizzle-kit push` to synchronize TypeScript schema definitions with a target database:

1. **Interactive terminal dependency.** When `drizzle-kit push` detects a missing column or table, it pauses and prompts the user in the terminal (`Do you want to truncate/drop?`). In headless CI/CD pipelines or Docker entrypoints where no interactive TTY exists, the command hangs or requires `--force`.
2. **Unsafe headless flags.** Passing `--force` automatically accepts all destructive operations. A typographical error in a schema file deployed through a deployment pipeline drops production columns immediately.
3. **External runtime requirement.** Drizzle requires Node.js, `npm`, or `pnpm` in the production container image, increasing image size and vulnerability surface.
4. **No pod coordination.** Drizzle is designed as a developer CLI tool. It does not provide distributed database locking to coordinate concurrent application instances starting up in a cluster.

### Atlas (`ariga.io/atlas`)

Atlas is a standalone schema management engine written in Go:

1. **Pre-flight verification.** Atlas validates migrations against a temporary "dev database" before running migrations on target environments.
2. **Advisory locking and linting.** Atlas acquires advisory locks and analyzes migration steps for high-risk operations (such as table-locking DDL or missing defaults).
3. **Operational trade-offs.** Atlas runs as an external binary or Docker container rather than an in-process Go library. Integrating Atlas into an application binary requires running separate pre-migration jobs or managing container sidecars. Several advanced linting checks require Atlas Cloud accounts or enterprise licenses.

### Ent (`entgo.io`)

Ent is a Go entity framework that provides declarative schema migration:

1. **Safe default drop policies.** Ent disables destructive column drops by default (`WithDropColumn(false)`) and allows users to restrict migration scope to specific tables (`WithTables(...)`).
2. **Coupling to code generation.** Schemas must be defined using Ent's Go DSL. Teams cannot use standard `schema.sql` files directly, preventing simple interoperability with tools like `sqlc` or raw SQL scripts.

## Grizzle safety invariants

Grizzle combines the safety features of Atlas and Ent with an in-process, zero-dependency Go implementation that uses standard SQL DDL as the single source of truth.

### Invariant 1: Distributed mutual exclusion

Grizzle prevents multi-pod race conditions by acquiring a transactional advisory lock before inspecting or modifying the database:

* On PostgreSQL, Grizzle calls `pg_advisory_xact_lock(lock_id)`. The lock is tied to the current transaction. If multiple pods boot at the same time, the first pod acquires the lock and runs the migration. Subsequent pods wait until the transaction commits, introspect the updated schema, see zero pending changes, and start their HTTP servers without error.
* If a pod crashes during migration, PostgreSQL releases the transactional advisory lock automatically.
* On SQLite, Grizzle relies on SQLite's native single-writer database lock within an explicit transaction.

### Invariant 2: Isolated pre-flight shadow compilation

Before executing any DDL on the live database, Grizzle verifies the user's `schema.sql`:

1. It creates an isolated shadow namespace (`_grizzle_shadow` on PostgreSQL, or an in-memory database on SQLite).
2. It executes `opts.SchemaSQL` inside the shadow namespace.
3. If the SQL contains syntax errors, invalid constraints, or conflicting names, compilation halts immediately with `ErrCompilationFailed`.
4. The live schema is never touched if shadow compilation fails.
5. In PostgreSQL, the shadow schema is cleaned up in a `defer` block.

### Invariant 3: Non-destructive default policy

All drop operations are disabled by default. If `opts.SchemaSQL` omits a table, column, index, or foreign key that exists in the live database, Grizzle refuses to execute and returns a `DestructiveViolationError`:

```go
err := grizzle.Sync(ctx, db, grizzle.Options{
    SchemaSQL: schemaSQL,
    // AllowDrop is false by default.
})
// If unmapped columns or tables exist in live DB:
// Returns *DestructiveViolationError listing all blocked operations.
```

To enable drops, callers must explicitly grant permission. Grizzle supports granular drop policies:

```go
opts := grizzle.Options{
    SchemaSQL: schemaSQL,
    AllowDrop: false, // General drops disabled
    AllowDropIndex: ptr(true), // Allow dropping obsolete indexes
    AllowDropColumn: ptr(false), // Explicitly forbid dropping columns
}
```

### Invariant 4: Protection of unmanaged tables and extensions

Production databases frequently host tables not defined in the application's primary `schema.sql`:

* PostGIS and extension metadata: `spatial_ref_sys`, `geometry_columns`, `geography_columns`.
* Background task queues: `asynq_tasks`, `pgboss_jobs`, `temporal_executions`.
* Legacy migration trackers: `schema_migrations`, `goose_db_version`, `flyway_schema_history`.

Grizzle guarantees these tables are never modified or dropped:

1. **Built-in extension filters.** Tables like `spatial_ref_sys` and `geometry_columns` are ignored by default.
2. **ExcludeTables glob patterns.** Callers can specify exact names or wildcards to protect external tables:

```go
opts := grizzle.Options{
    SchemaSQL: schemaSQL,
    ExcludeTables: []string{"asynq_*", "temporal_*", "audit_log_*"},
}
```

3. **IncludeTables whitelist.** Callers can restrict Grizzle to a specific subset of tables:

```go
opts := grizzle.Options{
    SchemaSQL: schemaSQL,
    IncludeTables: []string{"users", "accounts", "organizations"},
}
```

Any table outside `IncludeTables` is ignored during inspection, diffing, and drop detection.

### Invariant 5: Pre-execution hazard detection

Grizzle inspects planned migration steps and identifies operational risks before applying them. Callers inspect these risks using `Plan.Hazards()`:

```go
plan, err := grizzle.PlanDiff(ctx, db, opts)
if err != nil {
    log.Fatal(err)
}

for _, hazard := range plan.Hazards() {
    switch hazard.Level {
    case grizzle.HazardLevelCritical:
        log.Printf("CRITICAL HAZARD: %s on table %s", hazard.Description, hazard.Table)
    case grizzle.HazardLevelWarning:
        log.Printf("WARNING: %s on table %s", hazard.Description, hazard.Table)
    case grizzle.HazardLevelNotice:
        log.Printf("NOTICE: %s on table %s", hazard.Description, hazard.Table)
    }
}
```

Hazard severity levels:

* **CRITICAL**: Potential data loss. Triggered by `DROP TABLE`, `DROP COLUMN`, and destructive column type narrowing (such as converting `bigint` to `smallint`).
* **WARNING**: Execution failure on non-empty tables. Triggered by adding a `NOT NULL` column without a `DEFAULT` expression to an existing table.
* **NOTICE**: Operational impact. Triggered by index creations (which acquire table locks during build), index drops (which may affect query performance), and foreign key constraint removals.

### Invariant 6: Atomic execution

All generated DDL steps execute inside a single database transaction:

* If any statement fails (e.g. data validation error, type cast failure), the transaction rolls back.
* The database returns to its exact prior state.
* The application exits cleanly with an error without leaving partial schema changes.

## Recommended production usage pattern

For production services, combine `PlanDiff` in staging or CI with `Sync` on application boot:

```go
package main

import (
    "context"
    "database/sql"
    _ "embed"
    "fmt"
    "log"
    "os"

    _ "github.com/jackc/pgx/v5/stdlib"
    "github.com/yourorg/grizzle"
)

//go:embed schema.sql
var schemaSQL string

func main() {
    ctx := context.Background()
    db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
    if err != nil {
        log.Fatalf("connect failed: %v", err)
    }
    defer db.Close()

    opts := grizzle.Options{
        SchemaSQL: schemaSQL,
        AllowDrop: false, // Prevent all drops
        ExcludeTables: []string{"asynq_*", "temporal_*"},
    }

    // Inspect plan and verify no critical hazards exist
    plan, err := grizzle.PlanDiff(ctx, db, opts)
    if err != nil {
        log.Fatalf("plan diff failed: %v", err)
    }

    // Print readable plan to logs
    _ = plan.Format(os.Stdout, false)

    for _, h := range plan.Hazards() {
        if h.Level == grizzle.HazardLevelCritical {
            log.Fatalf("aborted boot: critical hazard detected: %s", h.Description)
        }
    }

    // Execute synchronization
    if err := grizzle.Sync(ctx, db, opts); err != nil {
        log.Fatalf("schema sync failed: %v", err)
    }

    fmt.Println("Database synchronized successfully.")
}
```
