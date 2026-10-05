# Grizzle architecture

This document details the architectural design, lifecycle, component interactions, and dependency rules of Grizzle.

## 1. Layered dependency rule

Grizzle follows a strict functional core, imperative shell design enforced across packages:

```
schema  <--  scope, diff, plan  <--  dialect  <--  exec, history  <--  grizzle (root facade)
```

| Layer | Packages | Responsibility | Purity Rule |
| :--- | :--- | :--- | :--- |
| **Model** | `internal/schema` | Pure relational model: `Table`, `Column`, `Index`, `Constraint`, `Schema`, normalization | **Pure**: No `database/sql`, no `context`, no I/O, no globals |
| **Logic** | `internal/scope`<br>`internal/diff`<br>`internal/plan` | Scope filtering, AST schema diffing, topological sort, `Plan.Hash()`, structural hazard analysis | **Pure**: Deterministic algorithms, sorted maps/slices for stable hashing |
| **Dialect** | `internal/dialect`<br>`internal/dialect/postgres`<br>`internal/dialect/sqlite` | SQL DDL rendering, catalog introspection queries, advisory locks, 12-step rebuild | Only layer with dialect-specific SQL syntax knowledge |
| **Execution** | `internal/exec`<br>`internal/history` | Connection management, shadow schemas, session/transaction locking, statement timeouts, retries, history audit log | Handles I/O, transactions, retries, and errors |
| **Facade** | root (`grizzle`) | Public API (`Sync`, `PlanDiff`, `Apply`, `Check`), options validation, public type aliases | Wiring only; zero core logic |

An automated test (`TestArchitecture_LayeredDependencies`) verifies that no package violates this one-way dependency rule.

## 2. High-level execution flow

```mermaid
flowchart TD
    subgraph Go Application Process [Go application process]
        AppMain["main() / boot hook"] --> SyncCall["grizzle.Apply / grizzle.Sync"]
        SyncCall --> ConnAcquire["1. Acquire dedicated DB connection"]
        ConnAcquire --> LockMgr["2. Session / transaction advisory lock"]
        LockMgr --> ShadowRunner["3. Shadow schema compilation"]
        ShadowRunner --> DiffEngine["4. Post-lock catalog introspection & diff"]
        DiffEngine --> HashVerify["5. Plan approval hash verification (ExpectedHash)"]
        HashVerify --> SafetyGuard["6. Safety policy & hazard gating (AcceptHazards)"]
        SafetyGuard --> PlanGrouper["7. Group steps: NonTx (CONCURRENTLY) vs Tx"]
        PlanGrouper --> Applier["8. Execute DDL + Record grizzle_history"]
        Applier --> Cleanup["9. Shadow cleanup & release lock"]
    end

    subgraph Database Server [Database server]
        AdvisoryLock[("Session / Tx Advisory Lock")]
        ShadowSchema[("Schema: _grizzle_shadow")]
        PublicSchema[("Schema: public (live data)")]
        HistoryTable[("Table: grizzle_history")]
    end

    LockMgr -.-> AdvisoryLock
    ShadowRunner -.-> ShadowSchema
    DiffEngine -.-> ShadowSchema
    DiffEngine -.-> PublicSchema
    Applier -.-> PublicSchema
    Applier -.-> HistoryTable
```

## 3. Detailed sequence: Locking, post-lock diffing, and execution

```mermaid
sequenceDiagram
    autonumber
    participant App as Go Application
    participant Conn as Dedicated Conn
    participant LiveDB as Live Database

    App->>Conn: Acquire dedicated connection & SET search_path
    App->>Conn: Apply session timeouts (LockTimeout, StatementTimeout)
    App->>Conn: SELECT pg_advisory_lock(lock_id)
    Note over App,Conn: Concurrent pods wait or retry on lock conflict (55P03)

    App->>Conn: BEGIN Shadow Compilation Transaction
    App->>Conn: CREATE SCHEMA _grizzle_shadow;
    App->>Conn: Exec(schema.sql in shadow)
    App->>Conn: Inspect shadow catalog (desired state)
    App->>Conn: Inspect live catalog (live state)
    App->>Conn: ROLLBACK Shadow Compilation Transaction

    Note over App: DiffEngine computes changes post-lock
    Note over App: Verify ExpectedHash matches Plan.Hash()
    Note over App: Enforce DropPolicy (hard-blocks unpermitted drops)
    Note over App: GateHazards (fails on unaccepted critical hazards)

    alt Non-Transactional Steps Exist (e.g. CREATE INDEX CONCURRENTLY)
        App->>Conn: Exec(CREATE INDEX CONCURRENTLY ...) on dedicated conn
    end

    App->>LiveDB: BEGIN Step Transaction
    App->>LiveDB: SET LOCAL search_path TO target_schema, public;
    App->>LiveDB: Exec(Transactional DDL steps in topological order)
    Note over LiveDB: Foreign keys added with NOT VALID, then VALIDATE CONSTRAINT
    App->>LiveDB: INSERT INTO grizzle_history (plan_hash, steps, applied_at, duration_ms)
    App->>LiveDB: COMMIT Step Transaction

    App->>Conn: SELECT pg_advisory_unlock(lock_id)
    App->>Conn: Close dedicated connection
    Note over LiveDB: Lock freed, waiting replicas proceed
```

## 4. Core components

### 4.1 Lock manager & retry loop
* **Mechanism**: PostgreSQL session advisory locks on dedicated connections for non-transactional operations (`CREATE INDEX CONCURRENTLY`), and transactional advisory locks (`pg_advisory_xact_lock`) for pure transactional plans.
* **Conflict handling**: If PostgreSQL returns `55P03` (`lock_not_available` or `lock_timeout`), Grizzle retries with truncated exponential backoff and randomized jitter up to `Options.MaxRetries`.
* **Connection safety**: Dedicated connections ensure that session locks are released in `defer` handlers or automatically cleaned up if the connection drops.

### 4.2 Shadow runner
* **Mechanism**: Compiles `SchemaSQL` in `_grizzle_shadow` (PostgreSQL) or `:memory:` (SQLite) to validate SQL constraints and syntax before touching production tables.
* **Safety**: Zero live tables are modified during compilation.

### 4.3 Catalog inspector & diff engine
* **Mechanism**: Inspects relational catalogs (`pg_catalog` or `sqlite_schema`) post-lock.
* **Pure AST Diffing**: Compares tables, columns, constraints, enums, and indexes. Sorts all outputs stably.
* **Expand/Contract Detection**: Unmapped dropped and added columns with matching types trigger `HazardRenameAmbiguous`. Explicit mappings in `Options.Renames` generate atomic `RENAME COLUMN` or staged non-destructive column additions when `ExpandContract: true`.

### 4.4 Hazard analyzer & gatekeeper
* **Purity**: Structural inspection without SQL string parsing.
* **Enforcement**: Blocks execution if any critical hazard (`DROP_TABLE`, `DROP_COLUMN`, `TYPE_NARROW`, `RENAME_AMBIGUOUS`) is not present in `Options.AcceptHazards`.
* **Policy Invariant**: `AllowDrop: false` is checked first and hard-blocks deletions regardless of hazard acceptance.

### 4.5 Execution engine & history recorder
* **Step Grouping**: Automatically splits migration plans into transactional blocks and non-transactional blocks (`CONCURRENTLY` index creations).
* **Safe Constraints**: Foreign keys are created as `ADD CONSTRAINT ... NOT VALID` and subsequently validated with `VALIDATE CONSTRAINT` to prevent prolonged exclusive table locks.
* **Audit Trail**: Every successfully applied plan writes a record to `grizzle_history` containing the deterministic plan hash, execution timestamp, duration, applied user, and steps JSON.
