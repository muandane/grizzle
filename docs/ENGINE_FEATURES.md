# Planned engine features

This document specifies features planned for the Grizzle migration engine. Each section describes the technical requirements, PostgreSQL or SQLite catalog interactions, execution sequencing, and safety hazards.

## PostgreSQL partitioned tables

*Status: Implemented*

Partitioned tables divide large tables into smaller physical tables while preserving a single logical table interface.

### Declarative syntax

The engine must parse and diff table partitioning declarations in standard SQL DDL:

```sql
CREATE TABLE measurements (
    city_id INT NOT NULL,
    log_date DATE NOT NULL,
    peak_temp INT,
    units_sold INT
) PARTITION BY RANGE (log_date);

CREATE TABLE measurements_y2026m01 PARTITION OF measurements
    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');

CREATE TABLE measurements_y2026m02 PARTITION OF measurements
    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
```

The engine must support range, list, and hash partitioning methods:
* `PARTITION BY RANGE (column_name)`
* `PARTITION BY LIST (column_name)`
* `PARTITION BY HASH (column_name)`

### Catalog introspection

PostgreSQL stores partitioning metadata across `pg_partitioned_table`, `pg_inherits`, and `pg_class`:

```sql
SELECT
    c.relname AS table_name,
    p.partstrat AS strategy,
    p.partnatts AS num_keys,
    pg_get_partkeydef(c.oid) AS partition_key
FROM pg_partitioned_table p
JOIN pg_class c ON c.oid = p.partrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.relname = $1;
```

To introspect attached partition bounds:

```sql
SELECT
    inhrelid::regclass::text AS child_table,
    inhparent::regclass::text AS parent_table,
    pg_get_expr(c.relpartbound, c.oid) AS partition_bounds
FROM pg_inherits i
JOIN pg_class c ON c.oid = i.inhrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.relname = $1;
```

### Safety hazards and invariants

Converting an existing standard table into a partitioned table requires rewriting all rows. The engine enforces these invariants:

1. Modifying a table from regular to partitioned cannot happen with `ALTER TABLE`. The engine rejects this transition with `ErrPartitionConversion` unless an explicit staged migration strategy is defined.
2. Detaching a partition preserves data in the child table.
   - On PostgreSQL 14+ (`server_version_num >= 140000`), detaching executes via `ALTER TABLE parent DETACH PARTITION child CONCURRENTLY;` as a standalone `NonTx` step outside transaction blocks to minimize table locks.
   - On PostgreSQL < 14, when `--non-concurrent-indexes` is specified, or when the partitioned table contains a `DEFAULT` partition, detaching automatically falls back to standard transactional `ALTER TABLE parent DETACH PARTITION child;`.
   - Interrupted detaches (pending-detach state in `pg_inherits.inhdetachpending`) are detected during catalog introspection and planned as `ALTER TABLE parent DETACH PARTITION child FINALIZE;`, accompanied by the `PARTITION_PENDING_DETACH` warning hazard.
3. Attaching an existing table with data (`ALTER TABLE parent ATTACH PARTITION child FOR VALUES ...`) requires a table scan to validate that rows fit the partition bound. In PostgreSQL, this acquires an `ACCESS EXCLUSIVE` lock on both parent and child. The step emits a `PARTITION_ATTACH_SCAN` hazard at `WARNING` level.
4. Structural uniqueness rules: Pure diff validates that any `PRIMARY KEY` or unique index on a partitioned table includes all partition key columns. Violations are rejected deterministically at plan time with `ErrPartitionKeyNotInUnique` before executing any database I/O.
5. Runtime foreign key and constraint execution errors on partitioned tables are wrapped with specific table name and constraint context.

---

## Partial and functional indexes

*Status: Implemented*

Partial and functional indexes reduce index storage and speed up targeted queries by indexing expressions or filtered row sets.

### Declarative syntax

The engine must support expression keys and `WHERE` filter predicates:

```sql
-- Functional index on lowercased email
CREATE UNIQUE INDEX idx_users_email_lower ON users (lower(email));

-- Partial index on active records only
CREATE INDEX idx_orders_pending ON orders (created_at) WHERE status = 'pending';
```

### Catalog introspection and expression normalization

PostgreSQL parses expressions into internal node trees and normalizes SQL representations upon catalog storage. Querying `pg_index` and `pg_get_expr` returns the canonical form:

```sql
SELECT
    c.relname AS index_name,
    i.indisunique AS is_unique,
    pg_get_expr(i.indpred, i.indrelid) AS predicate,
    pg_get_indexdef(i.indexrelid) AS index_def
FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid
JOIN pg_class t ON t.oid = i.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
WHERE n.relname = $1 AND t.relname = $2;
```

Comparing raw user SQL with `pg_get_indexdef` creates false diffs due to:
* Explicit type casts added by the PostgreSQL catalog (for example, `'pending'::text`).
* Parentheses wrapping expressions (for example, `(status = 'pending'::text)`).
* Fully qualified schema references on custom functions.

The engine compiles index definitions in the shadow schema and compares the output of `pg_get_indexdef` from the live catalog against `pg_get_indexdef` from the shadow catalog. This eliminates string parsing heuristics.
* **Non-immutable function detection:** When an index expression references non-immutable functions (e.g. `clock_timestamp()` or a `STABLE`/`VOLATILE` function), shadow compilation fails before modifying the live database. Grizzle wraps the pre-flight failure with the index name, table name, and `"expression must be IMMUTABLE"`.
* **Search path resolution:** Shadow compilation configures `search_path` to include both the shadow schema and the target schema (`SET LOCAL search_path TO shadow, target, public;`), allowing custom functions defined in external or target schemas to resolve correctly.

### Execution sequencing

Partial and functional indexes follow standard index creation rules:
1. Production execution uses `CREATE INDEX CONCURRENTLY`.
2. Failed index builds leaving `indisvalid = false` trigger `DROP INDEX CONCURRENTLY` followed by recreation.

---

## Declarative check constraints

Check constraints enforce row-level validation predicates on table columns.

### Declarative syntax

```sql
CREATE TABLE products (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    price_cents INT NOT NULL,
    discount_cents INT NOT NULL DEFAULT 0,
    CONSTRAINT check_positive_price CHECK (price_cents > 0),
    CONSTRAINT check_valid_discount CHECK (discount_cents >= 0 AND discount_cents <= price_cents)
);
```

### Catalog introspection

Introspect check constraints via `pg_constraint`:

```sql
SELECT
    conname AS constraint_name,
    conrelid::regclass::text AS table_name,
    pg_get_constraintdef(oid) AS definition,
    convalidated AS is_validated
FROM pg_constraint
WHERE contype = 'c' AND connamespace = $1::regnamespace;
```

### Non-blocking execution

Adding a check constraint with `CHECK (expr)` scans the entire table under `ACCESS EXCLUSIVE` lock, blocking read and write operations. The engine must stage check constraints across two transactions:

1. Add the constraint without validation in transaction group 1:
   ```sql
   ALTER TABLE products ADD CONSTRAINT check_positive_price CHECK (price_cents > 0) NOT VALID;
   ```
   This acquires an `ACCESS EXCLUSIVE` lock for catalog updates only, taking milliseconds regardless of table size.
2. Validate the constraint in a separate transaction group 2:
   ```sql
   ALTER TABLE products VALIDATE CONSTRAINT check_positive_price;
   ```
   This runs a sequential table scan under `SHARE UPDATE EXCLUSIVE` lock, permitting concurrent `SELECT`, `INSERT`, `UPDATE`, and `DELETE` queries.

---

## Multi-schema support

*Status: Implemented*

Large applications organize data across multiple PostgreSQL schemas, such as logical domains or tenant spaces.

### Scope and catalog changes

Current engine options accept a single `TargetSchema`. Multi-schema support accepts a list of target schemas via `TargetSchemas` (with `TargetSchema` retained as a deprecated alias):

```go
opts := grizzle.Options{
    TargetSchemas: []string{"public", "billing", "identity"},
    SchemaSQL:     schemaSQL,
}
```

The engine applies these rules:
1. **Search path configuration:** Shadow schemas mirror each declared schema namespace (`_grizzle_shadow_public`, `_grizzle_shadow_billing`). During shadow compilation, statements qualify target identifiers rewritten to their shadow counterparts without altering literals or comments.
2. **Cross-schema foreign keys:** Tables in `billing` referencing primary keys in `identity` resolve correctly during relational dependency topological sorting (`identity.users` created before `billing.accounts`; reverse order on drop).
3. **Lock identifiers:** The advisory lock hashing algorithm combines the database identifier with all declared schema names (`grizzle:<sorted_schemas>`) to prevent lock collisions across distinct applications sharing a database.

---

## Interactive terminal inspection

*Status: Implemented*

For local development outside automated CI pipelines, the CLI will support an interactive terminal mode.

### Behavior

1. When running `grizzle apply` without `--plan` in an interactive terminal (TTY attached):
   * The CLI renders a formatted table showing all planned steps, affected tables, and identified hazards.
   * If critical hazards exist, the CLI details the hazard code and data loss consequence.
   * The prompt requests explicit confirmation:
     ```text
     Planned changes:
       + CREATE TABLE accounts (id, balance)
       ! DROP COLUMN users.legacy_role [DROP_COLUMN: CRITICAL]

     Apply these changes? [y/N/details]:
     ```
2. When running in headless environments (no TTY attached):
   * Interactive prompts are disabled.
   * The command requires explicit flags (`--accept-hazard CODE`) and exits immediately with code 2 if unapproved hazards exist.

---

## SQLite table rebuild engine improvements

*Status: Implemented*

SQLite does not support altering column types, renaming foreign keys, or dropping constraints in place. The engine executes a 12-step table recreation procedure.

### Current 12-step rebuild procedure

1. Resolve table dependencies and check foreign keys.
2. Create temporary table `_grizzle_rebuild_table`.
3. Copy data from original table to temporary table.
4. Drop original table.
5. Rename temporary table to original name.
6. Recreate indexes and triggers.
7. Run `PRAGMA foreign_key_check`.

### Implemented improvements

1. **Trigger and view preservation:** Introspects triggers and views referencing the target table before dropping it. Rebinds them to the recreated table after renaming.
2. **Batch data copying for large tables:** Copying large datasets in a single `INSERT INTO ... SELECT` statement inflates SQLite journal memory. The engine performs chunked keyset copying (`WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`) when table row count exceeds `SQLiteRebuildThreshold`.
   - **Global threshold design:** `SQLiteRebuildThreshold` (default 100,000) and `SQLiteRebuildBatchSize` (default 10,000) are configured globally in `Options` and `ApplyOpts` rather than per-table. This design keeps schema definitions clean and declarative while ensuring uniform memory and journal bounds across all tables.
   - **Disk and NAS performance verification:** Keyset batch copy duration and WAL page growth across chunk sizes are benchmarked (`sqlite_bench_test.go`). Rotational mechanical disk (HDD) and NAS network filesystems (NFS/SMB) introduce high-latency fsync pauses and checkpoint stalls that cannot be automated in CI; these must be validated manually per the checklist below.
   - **Multi-schema scope:** Multi-schema configurations (`TargetSchemas > 1`) are explicitly out of scope for SQLite and return typed error `ErrUnsupportedMultiSchema`, as SQLite operates with a single attached database per connection.
3. **Savepoint isolation:** Wraps each table rebuild in an explicit `SAVEPOINT grizzle_rebuild`. If `PRAGMA foreign_key_check` discovers constraint violations, rolls back the savepoint and aborts migration before committing.

### Manual verification checklist

The following operational characteristics cannot be fully automated in continuous integration runners and require periodic manual verification:

1. **Mechanical-disk & NAS storage performance:** Keyset batch copy duration, WAL page checkpoint stalls, and journal growth under rotational disks (5400/7200 RPM) or high-latency network mounts (NFS v4, SMB).
2. **Multi-version live PostgreSQL matrix:** Concurrently executing migration suites against live, isolated PostgreSQL clusters running 14.x, 15.x, 16.x, and 17.x in production-equivalent network topologies.
3. **Human interactive terminal emulators:** Manual validation of interactive terminal prompts (`grizzle apply` with `[y/N/details]`) across standard human terminals (macOS Terminal, iTerm2, tmux, Windows Terminal) beyond automated `pty.Open()` pseudo-terminals.

---

## Dedicated session advisory lock

PostgreSQL disallows running `CREATE INDEX CONCURRENTLY` inside an explicit transaction block (`ERROR: 25001: CREATE INDEX CONCURRENTLY cannot run inside a transaction block`). Because migration plans frequently interleave non-transactional index creations with transactional table modifications, a transaction-level lock (`pg_advisory_xact_lock`) cannot span both phases.

Grizzle implements a dedicated session-level advisory lock architecture that coordinates multi-pod migrations safely across all transactional and non-transactional step groups.

### Architecture and connection lifecycle

1. **Dedicated connection pinning:** Grizzle obtains a dedicated connection (`db.Conn(ctx)`) exclusively reserved for the migration lifecycle.
2. **Per-schema deadlock-free ordering:**
   - Single-schema migrations acquire a 64-bit integer advisory lock computed from `grizzle.GenerateLockID(namespace, schema)`.
   - Multi-schema migrations acquire two-integer advisory locks `pg_try_advisory_lock(hash32(LockNamespace), hash32(schema))` for each target schema. Schemas are sorted and deduplicated prior to acquisition to guarantee consistent acquisition order across concurrent processes, preventing cross-schema deadlocks.
   - On mid-acquisition failure, all previously held locks are released in reverse order before returning an error.
   - `Options.LockNamespace` (default `"grizzle"`) isolates lock spaces between distinct applications sharing the same database.
3. **Non-blocking lock acquisition loop:** Instead of blocking indefinitely on `pg_advisory_lock` (which can create server-side lock wait queues and trigger deadlocks against PostgreSQL internal catalog locks during concurrent DDL), Grizzle queries `SELECT pg_try_advisory_lock(...)` on a polling ticker.
4. **Context-scoped lock timeout:** Because PostgreSQL server-side `SET lock_timeout` does not govern `pg_try_advisory_lock`, timeout enforcement is bounded client-side using `context.WithTimeout(ctx, lockTimeout)`. Timeout expiration yields `plan.ErrLockTimeout`, which triggers truncated exponential backoff retry with jitter up to `Options.MaxRetries`.
5. **Session hygiene and connection reset:** Because `*sql.Conn.Close()` returns connections to the `*sql.DB` pool rather than terminating TCP sockets, Grizzle releases advisory locks in reverse order via `SELECT pg_advisory_unlock(...)` and issues session-level resets (`RESET search_path; RESET lock_timeout; RESET statement_timeout;`) inside `defer` handlers.
6. **Direct plan apply parity:** When executing approved plans via `grizzle.Apply` where `SchemaSQL` is not retained, statements route through `exec.ApplyPostgres` / `exec.ApplySQLite` under dedicated session locks and step grouping (`GroupSteps`), rather than naive uncoordinated transactions.
7. **Multi-pod mutual exclusion and idempotency:** Under the advisory lock, `applyPostgresOnce` and `ApplySQLite` verify `history.IsApplied(ctx, conn, dialect, schema, planHash)`. If a competing replica has already committed the plan under the lock, subsequent replicas exit gracefully as a no-op instead of failing on duplicate relation creation.
