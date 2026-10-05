# Database migration safety in production

Grizzle applies declarative database migrations directly inside the application process on boot. This document evaluates how Grizzle, Drizzle ORM, Atlas, Ent, and TypeORM handle schema updates in production environments, and documents the safety invariants Grizzle enforces in code.

## Production safety comparison

Production database migrations face five core failure modes: concurrent replica execution races, silent data loss from dropped columns or tables, runtime errors from incompatible column modifications, accidental deletion of unmanaged tables (such as PostGIS metadata or background worker queues), and zero-downtime column migration ambiguities.

The following table compares how each tool handles these failure modes:

| Capability | TypeORM (`synchronize`) | Drizzle (`drizzle-kit push`) | Atlas CLI | Ent (`entgo.io`) | Grizzle |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Runtime mode** | In-process | External Node.js CLI | External Go binary / Docker | In-process | In-process |
| **Schema definition** | TypeScript entities | TypeScript schema files | HCL or SQL | Go structs | Standard `schema.sql` |
| **Distributed locking** | None (races on multi-pod boot) | None (single-process CLI) | Transactional advisory locks | Custom / driver-dependent | `pg_advisory_xact_lock` & session lock with post-lock re-diffing |
| **Pre-flight shadow validation** | None | None (executes on target) | Dev database / Docker container | In-memory parser | Isolated shadow schema / in-memory SQLite |
| **Default drop behavior** | Drops unmapped tables/columns | Prompts in TTY; drops with `--force` | Configurable via flags | Drops disabled (`WithDropColumn(false)`) | Drops hard-blocked (`AllowDrop: false`) |
| **Granular drop controls** | None | None | Per-resource policies in HCL | Table and column flags | Separate table, column, index, FK flags |
| **Unmanaged table protection** | None (drops third-party tables) | None | `exclude` pattern list | `WithTables(...)` whitelist | `StrictScope`, `IncludeTables`, `ExcludeTables`, extension filters |
| **Pre-execution hazard analysis** | None | None | Analyzer rules (Pro/Cloud for advanced) | None | Built-in structural `Plan.Hazards()` with blocking hazard gate |
| **Plan approval & drift check** | None | None | Migration directory integrity checks | None | Deterministic `Plan.Hash()` + `ExpectedHash` + read-only `Check()` |
| **Timeouts & conflict retry** | Driver defaults | Driver defaults | Configurable client timeouts | Driver defaults | `LockTimeout`, `StatementTimeout`, exponential backoff retry |
| **Expand / Contract migrations** | None | None | Versioned migration files | Custom workflows | `HazardRenameAmbiguous`, `Renames` map, staged `ExpandContract` |
| **Execution atomicity** | Partial (statement-by-statement) | Transactional where supported | Transactional | Transactional | Transactional groups with non-tx (`CONCURRENTLY`) support |

## Detailed analysis of existing tools

### TypeORM (`synchronize: true`)

TypeORM includes a `synchronize` flag intended for rapid local development. As documented in the [official TypeORM documentation](https://typeorm.io/data-source-options) (as of v0.3.x):
> *"synchronize - Indicates if database schema should be auto created on every application launch. Be careful with this option and don't use this in production - otherwise you can lose production data."*

In production environments, running with `synchronize: true` introduces severe operational risks:

1. **No distributed locking.** When multiple Kubernetes pods boot simultaneously, each instance introspects the database concurrently, computes identical or overlapping DDL changes, and executes concurrent `ALTER TABLE` statements. This causes PostgreSQL deadlocks (`deadlock detected`), lock acquisition failures, and failed deployments.
2. **Silent data loss.** If an entity property is removed, renamed, or temporarily commented out, TypeORM generates `ALTER TABLE ... DROP COLUMN` or drops entire tables on startup without requiring confirmation or hazard acknowledgment.
3. **No shadow validation.** Statements are executed directly against the live database without prior compilation in a scratch schema. A failing constraint midway through execution leaves tables in a partially altered or corrupted state.
4. **Unmanaged table deletion.** TypeORM treats any database table not represented by an entity in the running application as obsolete, dropping tables belonging to worker queues (`asynq`, `pgboss`, `temporal`) or database extensions (`spatial_ref_sys`).

### Drizzle ORM (`drizzle-kit push`)

Drizzle ORM provides `drizzle-kit push` for rapid schema prototyping against development databases. As documented in the [official Drizzle Kit documentation](https://orm.drizzle.team/kit-docs/commands#push) (as of Drizzle Kit v0.30+):

1. **Interactive terminal dependency.** When `drizzle-kit push` detects schema truncations or deletions, it prompts interactively in the terminal (`Do you want to truncate/drop?`). In headless CI/CD pipelines, Kubernetes init containers, or Docker entrypoints where no interactive TTY is attached, execution blocks unless bypassed.
2. **Unsafe headless flags.** In non-interactive environments, passing `--force` unconditionally approves all destructive changes. A typo or omitted column definition in a deployed schema file immediately drops production data without granular policy enforcement.
3. **External runtime requirement.** Running Drizzle requires Node.js, `npm`, or `pnpm` inside production container images, expanding image footprints and increasing the attack surface.
4. **No multi-pod coordination.** `drizzle-kit push` is designed as a developer CLI tool. It lacks distributed advisory locking to synchronize multiple application replicas starting up concurrently. (For production deployments, Drizzle officially recommends generating discrete SQL migration files via `drizzle-kit generate` and executing them via `drizzle-kit migrate`).

### Atlas (`ariga.io/atlas`)

Atlas is an open-source schema management engine written in Go. As documented in the [official Atlas documentation](https://atlasgo.io/lint/analyzers) and [Atlas dev-database guides](https://atlasgo.io/concepts/dev-database) (as of Atlas v0.28+):

1. **Pre-flight verification.** Atlas validates migrations against a temporary "dev database" (either a local SQLite instance, an ephemeral PostgreSQL container, or a cloud dev database) before executing statements against target environments.
2. **Advisory locking and linting.** Atlas acquires database advisory locks and checks migration steps for high-risk operations (such as table-locking DDL or missing column defaults).
3. **Operational trade-offs.** 
   - Atlas runs as a standalone CLI binary or Docker container rather than an in-process Go library. Incorporating Atlas into a Go application requires maintaining pre-migration CI steps, Kubernetes init containers, or container sidecars.
   - Core CLI capabilities are licensed under the Business Source License (BSL 1.1) and Ariga Community License. While basic analyzers are available in the open tier, advanced analysis features—such as data-dependent checks, backward-compatibility validation across rolling releases, and team governance policies—require Atlas Pro or Atlas Cloud integration.

### Ent (`entgo.io`)

Ent is an entity framework for Go providing declarative schema migration. As documented in the [official Ent migration documentation](https://entgo.io/docs/migrate/#drop-assets) (as of Ent v0.14+):

1. **Safe default drop policies.** Ent disables destructive column drops by default (`schema.WithDropColumn(false)`) and allows users to restrict migration scope to specific tables using `schema.WithTables(...)`.
2. **Coupling to code generation.** Schemas must be authored using Ent's Go DSL (`ent/schema`). Teams cannot use plain `schema.sql` files directly, preventing direct interoperability with SQL-centric tooling such as `sqlc` or raw DDL scripts.
3. **No staged expand-and-contract.** Ent does not provide built-in ambiguous column rename detection or automated staged expand-and-contract primitives from declarative SQL DDL.

## Grizzle safety invariants

Grizzle combines the safety features of dedicated migration engines with an in-process, zero-dependency Go implementation that uses standard SQL DDL as the single source of truth.

### Invariant 1: Distributed mutual exclusion with post-lock re-diffing

Grizzle prevents multi-pod race conditions by acquiring advisory locks before inspecting or modifying the database:

* **PostgreSQL**: Grizzle acquires `pg_advisory_xact_lock(lock_id)` inside the active migration transaction (or a dedicated session-level advisory lock when executing non-transactional statements such as `CREATE INDEX CONCURRENTLY`).
* **Post-lock re-diffing**: Grizzle inspects the database and computes the diff **after** acquiring the lock. Even if an initial plan was generated prior to lock acquisition, Grizzle re-verifies the live schema post-lock, eliminating Time-of-Check to Time-of-Use (TOCTOU) schema drift.
* **Waiting pod behavior**: Subsequent pods wait on the advisory lock. Once the first pod commits, waiting pods acquire the lock, inspect the updated schema, discover zero pending diffs, and start immediately without executing redundant statements.
* **Failure recovery**: If a pod crashes or disconnects during migration, PostgreSQL automatically releases the advisory lock.
* **SQLite**: Synchronizes within an exclusive transaction with foreign keys verified via `PRAGMA foreign_key_check`.

### Invariant 2: Isolated pre-flight shadow compilation

Before executing any DDL on the live database, Grizzle compiles the user's `schema.sql`:

1. It creates an isolated shadow namespace (`_grizzle_shadow` on PostgreSQL, or an in-memory database on SQLite).
2. It executes `opts.SchemaSQL` inside the shadow namespace.
3. If the SQL contains syntax errors, invalid constraints, or conflicting names, compilation halts immediately with `ErrCompilationFailed`.
4. The live schema is never touched if shadow compilation fails.
5. In PostgreSQL, the shadow schema is guaranteed to be cleaned up via deferred cleanup handlers.

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
    SchemaSQL:       schemaSQL,
    AllowDrop:       false,           // General drops disabled
    AllowDropIndex:  grizzle.Ptr(true),  // Allow dropping obsolete indexes
    AllowDropColumn: grizzle.Ptr(false), // Explicitly forbid dropping columns
}
```

### Invariant 4: Blocking hazard gating

Grizzle statically inspects planned migration steps and identifies operational and data-loss risks before execution. Hazards are structural properties of migration steps—**Grizzle never relies on SQL string matching for safety logic**.

Critical hazards unconditionally block execution unless explicitly whitelisted in `Options.AcceptHazards`:

```go
plan, err := grizzle.PlanDiff(ctx, db, opts)
if err != nil {
    log.Fatal(err)
}

// Applying without accepting critical hazards returns ErrHazardBlocked
err = grizzle.Apply(ctx, db, plan, grizzle.ApplyOpts{
    AcceptHazards: []grizzle.HazardCode{
        grizzle.HazardDropColumn,
    },
})
```

Supported hazard codes:

| Hazard Code | Level | Condition |
| :--- | :--- | :--- |
| `DROP_TABLE` | `CRITICAL` | Table dropped from schema (permanent data loss) |
| `DROP_COLUMN` | `CRITICAL` | Column dropped from table (permanent data loss) |
| `TYPE_NARROW` | `CRITICAL` | Column type narrowed (e.g. `bigint` to `integer`, risk of numeric overflow) |
| `RENAME_AMBIGUOUS` | `CRITICAL` | Unmapped column dropped and added with identical type in same table |
| `NOT_NULL_NO_DEFAULT` | `WARNING` | Adding non-null column without default to non-empty table |
| `DROP_INDEX` | `NOTICE` | Index removal impacting query performance |
| `DROP_FK` | `NOTICE` | Foreign key constraint removal |

> [!IMPORTANT]
> `AllowDrop: false` is a hard safety invariant enforced at the policy gate before hazard evaluation. Setting `AcceptHazards: []HazardCode{HazardDropColumn}` will not bypass a disabled drop policy.

### Invariant 5: Plan/Apply split with approval hash

For production deployments requiring human-in-the-loop review or strict CI/CD gatekeeping, Grizzle supports a decoupled Plan and Apply workflow:

1. **Deterministic Plan Hash**: `Plan.Hash()` calculates a SHA-256 digest over the canonical list of migration steps, scope configurations, and rename mappings.
2. **Approval Verification**:
   ```go
   // In CI / Review phase:
   plan, _ := grizzle.PlanDiff(ctx, db, opts)
   approvedHash := plan.Hash()

   // In Production Deployment phase:
   err := grizzle.Apply(ctx, db, plan, grizzle.ApplyOpts{
       ExpectedHash: approvedHash,
   })
   ```
3. **Drift Abort**: If concurrent modifications altered the database between plan approval and deployment, the recomputed post-lock hash will differ, and `Apply` immediately aborts with `ErrPlanDrift`.

### Invariant 6: Timeouts and retry with exponential backoff

To prevent blocking production workloads during lock contention, Grizzle enforces strict timeout budgets:

* `LockTimeout`: Configurable session/transaction lock acquisition timeout (defaults to 5 seconds).
* `StatementTimeout`: Maximum allowed duration for any individual DDL statement (defaults to 5 minutes).
* **Automatic Retry**: If PostgreSQL returns a lock timeout error (SQLSTATE `55P03`), Grizzle automatically retries acquisition using exponential backoff with randomized jitter up to `MaxRetries` (defaults to 5 retries).

### Invariant 7: Strict scope protection

Production databases frequently host tables not defined in the application's primary `schema.sql`:

* PostGIS and extension metadata: `spatial_ref_sys`, `geometry_columns`, `geography_columns`.
* Background task queues: `asynq_tasks`, `pgboss_jobs`, `temporal_executions`.
* Legacy migration trackers: `schema_migrations`, `goose_db_version`, `flyway_schema_history`.

Grizzle guarantees these tables are never modified or dropped:

1. **Built-in extension filters.** Common extension tables are ignored automatically.
2. **StrictScope Mode**: For zero-blast-radius production environments, enabling `Options.StrictScope: true` mandates a non-empty `IncludeTables` whitelist. If missing, Grizzle fails immediately with `ErrStrictScope`.
3. **ExcludeTables**: Wildcard glob patterns (e.g. `asynq_*`, `temporal_*`) protect external systems.

### Invariant 8: Staged expand-and-contract column renames

Dropping and re-adding columns of the same type is an ambiguous operation that can lead to catastrophic data loss if executed naively.

1. **Ambiguous Detection**: When an unmapped column is dropped and another of the same type is added, Grizzle flags the step with `HazardRenameAmbiguous` (`CRITICAL`), halting migration.
2. **Explicit Rename Mapping**: Callers explicitly map renames via `Options.Renames`:
   ```go
   opts := grizzle.Options{
       Renames: map[string]string{
           "users.first_name": "given_name",
       },
   }
   ```
3. **Single-Step Atomic Rename**: By default, explicit renames generate `ALTER TABLE ... RENAME COLUMN`.
4. **Staged Expand and Contract**: For zero-downtime rolling deployments, enabling `Options.ExpandContract: true` executes the expand phase:
   - Adds the new column alongside the existing column without dropping the old column.
   - The application can dual-write and backfill data while both old and new application versions run.
   - The destructive contract step (dropping the old column) is deferred to a subsequent, separately approved plan.

### Invariant 9: Migration history and read-only drift detection

Grizzle maintains an immutable audit log and provides drift inspection:

* **History Tracking**: Successfully applied plans are recorded in `grizzle_history` with the plan hash, execution timestamp, duration, applied user, and serialized steps JSON—committed in the same transaction as the schema changes where possible.
* **Read-Only Drift Check**: `grizzle.Check(ctx, db, opts)` inspects the live database without acquiring exclusive write locks or executing mutations. If any schema drift exists, it returns `ErrDrift` (carrying the diff details in `*plan.DriftError`).

---

## Recommended production usage pattern

For production services, combine `PlanDiff` in staging or CI with `Apply` on application boot:

```go
package main

import (
    "context"
    "database/sql"
    _ "embed"
    "fmt"
    "log"
    "os"
    "time"

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
        SchemaSQL:        schemaSQL,
        StrictScope:      true,
        IncludeTables:    []string{"users", "accounts", "organizations"},
        ExcludeTables:    []string{"asynq_*", "temporal_*"},
        LockTimeout:      5 * time.Second,
        StatementTimeout: 2 * time.Minute,
        MaxRetries:       5,
    }

    // 1. Verify schema drift before boot
    if err := grizzle.Check(ctx, db, opts); err != nil {
        log.Printf("drift detected: %v", err)
    }

    // 2. Plan schema synchronization
    plan, err := grizzle.PlanDiff(ctx, db, opts)
    if err != nil {
        log.Fatalf("plan diff failed: %v", err)
    }

    // Print readable plan to logs
    _ = plan.Format(os.Stdout, false)

    // Verify no unaccepted critical hazards exist
    for _, h := range plan.Hazards() {
        if h.Level == grizzle.HazardLevelCritical {
            log.Fatalf("aborted boot: unapproved critical hazard detected: %s", h.Description)
        }
    }

    // 3. Apply approved plan with plan hash verification
    err = grizzle.Apply(ctx, db, plan, grizzle.ApplyOpts{
        ExpectedHash: plan.Hash(),
    })
    if err != nil {
        log.Fatalf("schema sync failed: %v", err)
    }

    fmt.Println("Database synchronized successfully.")
}
```
