# Grizzle specification

This document defines the technical specification, API contract, and safety model for Grizzle.

## 1. Scope and engine compatibility

* **Language**: Go 1.27+
* **Engines supported**:
  * PostgreSQL 14, 15, 16, 17, 18 (tested in CI; 13 claim removed — untested)
  * SQLite 3.35+ (via pure-Go `modernc.org/sqlite`, zero Cgo)
* **Database drivers supported**:
  * PostgreSQL: `github.com/jackc/pgx/v5/stdlib`, `github.com/lib/pq`
  * SQLite: `modernc.org/sqlite`, `github.com/mattn/go-sqlite3`
* **Dependencies**: Pure Go standard library and driver interfaces. Zero external CLI binaries or Docker containers.

## 2. API contract

### 2.1 Primary functions

```go
// Sync synchronizes the live database schema to match opts.SchemaSQL.
func Sync(ctx context.Context, db *sql.DB, opts Options) error

// PlanDiff computes the migration plan without executing any changes on the target database.
func PlanDiff(ctx context.Context, db *sql.DB, opts Options) (*Plan, error)

// Apply applies an approved migration plan to the database.
// If ExpectedHash is provided and the plan recomputed post-lock differs, Apply aborts with ErrPlanDrift.
func Apply(ctx context.Context, db *sql.DB, p *Plan, opts ApplyOpts) error

// Check inspects the live database and returns ErrDrift if the schema differs from opts.SchemaSQL.
// It is strictly read-only and never modifies the database.
func Check(ctx context.Context, db *sql.DB, opts Options) error

// Export generates migration artifacts for the plan in the requested format (sql, goose, atlas).
func Export(p *Plan, format ExportFormat, version string) ([]Artifact, error)
```

### 2.2 Configuration options

```go
type Options struct {
    // Dialect explicitly defines the database engine (DialectPostgres or DialectSQLite).
    // If empty, Grizzle detects the dialect automatically from the driver type.
    Dialect Dialect

    // SchemaSQL contains the complete desired state. PostgreSQL role and
    // catalog statements may share this file; they bypass shadow compilation
    // and are merged with the optional side-channel files.
    // Typically embedded at build time with //go:embed schema.sql.
    SchemaSQL string

    // TargetSchema is the schema to manage (defaults to "public" for Postgres, "main" for SQLite).
    // Deprecated: Use TargetSchemas for multi-schema support.
    TargetSchema string

    // TargetSchemas specifies the database schemas to manage (defaults to [TargetSchema] or ["public"] for Postgres).
    // For SQLite, entries other than "main" are ATTACH DATABASE names listed in SQLiteAttach.
    TargetSchemas []string

    // SQLiteAttach maps ATTACH DATABASE schema names to filesystem paths.
    // Every TargetSchemas entry other than "main" must appear here; "main" is
    // the primary database already open and must not be listed. Unknown keys,
    // empty paths, and duplicate names are rejected. Ignored on PostgreSQL.
    SQLiteAttach map[string]string

    // ShadowSchema is the temporary schema name used for validation (defaults to "_grizzle_shadow").
    // Must use the reserved "_grizzle_shadow" prefix, be a valid identifier of at most 63 bytes,
    // and must not name a target or included schema; violations fail with ErrInvalidOptions.
    ShadowSchema string

    // AllowDrop permits all destructive operations when set to true.
    // Defaults to false for zero data loss.
    AllowDrop bool

    // AllowRevoke permits privilege revocation when a RolesSQL grant is
    // removed. Has no granular override; the REVOKE_PRIVILEGE critical
    // hazard still requires AcceptHazards.
    AllowRevoke bool

    // AllowDropRole permits dropping a managed role that left RolesSQL.
    // Has no granular override; the DROP_ROLE critical hazard still
    // requires AcceptHazards.
    AllowDropRole bool

    // Granular drop overrides (nil inherits from AllowDrop):
    AllowDropTable     *bool
    AllowDropColumn    *bool
    AllowDropIndex     *bool
    AllowDropFK        *bool
    AllowDropCheck     *bool
    AllowDropExtension *bool
    AllowDropFunction  *bool
    AllowDropPolicy    *bool
    AllowDropTrigger   *bool
    AllowDropView      *bool
    AllowDropDomain    *bool

    // ExcludeTables defines table names or glob patterns (e.g. "spatial_ref_sys", "asynq_*")
    // that Grizzle will never alter, diff, or drop.
    ExcludeTables []string

    // IncludeTables restricts management scope to only the specified tables or patterns.
    IncludeTables []string

    // StrictScope requires IncludeTables to be non-empty when true, preventing accidental unmanaged operations.
    StrictScope bool

    // AcceptHazards specifies explicitly accepted critical hazards.
    // Unaccepted critical hazards block execution with ErrHazardBlocked.
    AcceptHazards []HazardCode

    // NonConcurrentIndexes forces PostgreSQL index creations to run transactionally without CONCURRENTLY.
    NonConcurrentIndexes bool

    // LockNamespace specifies application namespace string for PostgreSQL advisory locking (defaults to "grizzle").
    LockNamespace string

    // LockID is an optional explicit 64-bit integer used for the PostgreSQL advisory lock.
    // If 0, Grizzle derives 2-int per-schema advisory locks using (hash32(LockNamespace), hash32(schema)).
    LockID int64

    // LockTimeout sets the maximum TOTAL duration to wait for acquiring the advisory
    // lock across all retry attempts, including backoff (defaults to 5s). The value is
    // never reset per attempt; it also feeds the per-statement DDL lock_timeout.
    LockTimeout time.Duration

    // StatementTimeout sets the maximum duration for any individual DDL statement (defaults to 5m).
    StatementTimeout time.Duration

    // MaxRetries specifies maximum retry attempts when encountering lock_timeout (defaults to 3).
    MaxRetries int

    // RandFloat provides an optional random source func returning in [0.0, 1.0) for deterministic jitter in tests.
    RandFloat func() float64

    // Renames maps old column names to new column names (e.g. "users.old_col": "new_col")
    // to disambiguate renames instead of treating them as DROP + ADD.
    // Experimental: mapping format may change before 1.0.
    Renames map[string]string

    // ExpandContract enables staged expand-and-contract zero-downtime migrations.
    // Experimental: staged plan shape may change before 1.0.
    ExpandContract bool

    // Backfill hook is executed during staged expand migration outside the DDL lock window in batches.
    // Experimental: library-only; no CLI equivalent.
    Backfill BackfillFunc

    // BeforeSync runs once before any migration steps or locks execute; a
    // non-nil error aborts the migration before any DDL runs.
    BeforeSync SyncHook

    // AfterSync runs once after all migration steps and history recording succeed.
    AfterSync SyncHook

    // BeforeStep executes immediately prior to each plan step.
    BeforeStep StepHook

    // AfterStep executes immediately after each successful plan step.
    AfterStep StepHook

    // DryRun outputs planned statements without executing them.
    DryRun bool

    // DryRunLockTimeout bounds lock waits for live dry-run verification.
    DryRunLockTimeout time.Duration

    // SeedSQL contains idempotent data-seed SQL executed after a successful
    // sync (DDL -> AfterSync -> Seed); already-applied seeds are skipped
    // unless SeedForce is set.
    SeedSQL string

    // SeedForce re-runs the seed even when the same seed hash was already applied.
    SeedForce bool

    // RolesSQL is an optional PostgreSQL side-channel overlay for
    // SchemaSQL. Its role/grant entries override duplicate identities
    // extracted from SchemaSQL. Role statements are never shadow-compiled;
    // they are statement-scanned, diffed against live pg_authid roles and
    // object ACLs, and applied after all schema DDL. Only roles stamped with
    // the grizzle-managed marker comment are ever dropped, and only behind
    // AllowDropRole. Rejected for SQLite (ErrInvalidOptions).
    RolesSQL string

    // SQLiteRebuildThreshold defines row count threshold above which SQLite table rebuilds chunk data copying by keyset.
    SQLiteRebuildThreshold int

    // SQLiteRebuildBatchSize defines chunk size when copying data in batches during SQLite table rebuilds.
    SQLiteRebuildBatchSize int

    // Logger accepts a structured logger (*slog.Logger) for migration events.
    Logger *slog.Logger

    // Tracer receives lifecycle spans (sync start/end, step execution, lock wait).
    Tracer Tracer
}
```

#### RolesSQL contract (PostgreSQL)

`RolesSQL` is an optional side-channel overlay (CLI: `--roles roles.sql`) for
the desired role and privilege state. The same role statements may appear in
`SchemaSQL`; both inputs are statement-scanned and never shadow-compiled.
When a role or grant identity is present in both inputs, `RolesSQL` is
authoritative. Schema DDL remains in the shadow-compiled portion of
`SchemaSQL`.

```sql
-- roles.sql or SchemaSQL — these role statement forms are accepted:
CREATE ROLE app_read;                    -- default NOLOGIN when LOGIN/NOLOGIN omitted
CREATE USER app_writer WITH NOLOGIN;     -- USER is an alias for CREATE ROLE
CREATE ROLE app_login LOGIN PASSWORD 'secret' VALID UNTIL '2040-01-01' CONNECTION LIMIT 4;
ALTER ROLE app_login SET search_path = public;
ALTER ROLE app_login SET work_mem FROM CURRENT;
ALTER ROLE app_login RESET log_statement;
GRANT SELECT, INSERT ON docs TO app_read;
GRANT ALL ON TABLE docs TO app_read WITH GRANT OPTION;
GRANT USAGE ON SEQUENCE docs_id_seq TO app_read;
GRANT CONNECT ON DATABASE app TO app_read;
GRANT USAGE ON SCHEMA public TO app_read;
GRANT EXECUTE ON FUNCTION notify_event() TO app_read;
REVOKE INSERT ON docs FROM app_read;
```

Semantics:

- Statement scan rejects anything else (`ErrInvalidOptions`) — no silent
  ignoring. Other `SchemaSQL` statements remain in the shadow-compiled
  portion rather than being treated as RolesSQL.
- Managed role attributes: `LOGIN`/`NOLOGIN`, `PASSWORD` (plaintext literal
  only; `PASSWORD NULL` refused), `VALID UNTIL`, `CONNECTION LIMIT`,
  `INHERIT`/`NOINHERIT`, `CREATEDB`/`NOCREATEDB`, `CREATEROLE`/`NOCREATEROLE`,
  and `ALTER ROLE ... SET`/`RESET` (role-level `pg_db_role_setting`,
  `setdatabase = 0`). `SUPERUSER`, `REPLICATION`, and `BYPASSRLS` are refused.
  `DROP ROLE/USER` remains unsupported in the declarative contract.
- Passwords are compared by hash equality against `pg_authid.rolpassword`
  (md5/scram). Plaintext never appears in `Step.SQL`, plan JSON, or logs;
  apply rebuilds password SQL from parsed IR. Password drift emits
  `PASSWORD_CHANGE` (WARNING).
- When `LOGIN`/`NOLOGIN` is omitted, CREATE keeps PostgreSQL's default
  (`NOLOGIN`). Declared roles are stamped with a `grizzle-managed` catalog
  comment. Only marker-stamped roles absent from the desired state are
  dropped, and only behind `AllowDropRole` + `DROP_ROLE` (CRITICAL). A managed
  role that owns cluster objects aborts the sync with an ownership error.
- Grants are diffed per (object kind, object, grantee): missing → `GRANT`;
  surplus → `REVOKE` behind `AllowRevoke` + `REVOKE_PRIVILEGE` (CRITICAL);
  grant-option drift → `GRANT ... WITH GRANT OPTION` / `REVOKE GRANT OPTION FOR ...`.
- Grants to `PUBLIC` or to roles Grizzle does not manage are never revoked;
  a desired `GRANT ... TO PUBLIC` emits `GRANT_PUBLIC` (WARNING).
- `AllowRevoke` / `AllowDropRole` inherit from `AllowDrop` or can be set
  directly on `Options`; both round-trip through the plan artifact
  (approval-sensitive). Unqualified `TABLE`/`SEQUENCE` objects resolve
  against the primary target schema.
- Role steps sort after all schema DDL; the original `SchemaSQL` and
  `RolesSQL` strings each participate in `Plan.Hash()` when non-empty.
  Extracted statements are not hashed a second time. SQLite + non-empty
  `RolesSQL` is rejected.

#### CatalogSQL contract (PostgreSQL)

`CatalogSQL` is an optional side-channel overlay (CLI: `--catalog catalog.sql`)
for the desired cluster-catalog state of logical-replication publications,
DDL event triggers, subscriptions, and standalone logical replication slots.
The same catalog statements may appear in `SchemaSQL`; both inputs bypass
shadow compilation. When a catalog object name is present in both inputs,
`CatalogSQL` is authoritative. Catalog DDL cannot run inside the
shadow-compile transaction model.

```sql
-- catalog.sql or SchemaSQL — these catalog statement forms are accepted:
CREATE PUBLICATION docs_pub FOR TABLE docs, comments;
CREATE PUBLICATION analytics_pub FOR TABLES IN SCHEMA analytics; -- PostgreSQL 15+
CREATE PUBLICATION all_pub FOR ALL TABLES;
CREATE PUBLICATION ins_only WITH (publish = 'insert');           -- defaults to all four
ALTER PUBLICATION docs_pub ADD TABLE audit;
DROP PUBLICATION old_pub;
CREATE SUBSCRIPTION docs_sub CONNECTION 'host=publisher dbname=pub' PUBLICATION docs_pub;
-- WITH defaults: enabled=true, copy_data=true, create_slot=true, slot_name=<subscription name>
-- create_slot / copy_data are create-time only (not altered on existing subscriptions)
ALTER SUBSCRIPTION docs_sub CONNECTION 'host=publisher dbname=pub password=secret';
ALTER SUBSCRIPTION docs_sub ENABLE;
ALTER SUBSCRIPTION docs_sub SET PUBLICATION docs_pub, audit_pub;
DROP SUBSCRIPTION old_sub;
SELECT pg_create_logical_replication_slot('docs_slot', 'pgoutput');
SELECT pg_drop_replication_slot('old_slot');
CREATE EVENT TRIGGER audit_ddl ON ddl_command_end EXECUTE FUNCTION log_ddl();
CREATE EVENT TRIGGER block_drop ON sql_drop WHEN TAG IN ('DROP TABLE') EXECUTE FUNCTION refuse_ddl();
ALTER EVENT TRIGGER audit_ddl DISABLE;
DROP EVENT TRIGGER old_audit;
```

Semantics:

- Statement scan rejects anything else (`WHEN VALUE IN`, schema DDL,
  physical slots, unsupported subscription `WITH` options —
  `ErrInvalidOptions`); no silent ignoring. Other `SchemaSQL` statements
  remain in the shadow-compiled portion rather than being treated as
  CatalogSQL. `SELECT pg_create_logical_replication_slot` / `pg_drop_replication_slot`
  are stripped into CatalogSQL (exempt from shadow compile and L009).
- `ALTER` statements require a matching declaration across the unified file
  and `CatalogSQL`; a side-channel `CREATE` replaces a same-name unified
  declaration before its side-channel `ALTER`/`DROP` operations are replayed.
  Operation-only inputs do not imply drops of unrelated managed catalog objects.
- Publications are diffed on membership and `publish` flags from
  `pg_publication` / `pg_publication_rel` / `pg_publication_namespace`:
  missing → create, drift → `ALTER PUBLICATION`, drop → gated by
  `AllowDropPublication` + `DROP_PUBLICATION` (CRITICAL). Unqualified
  `TABLE` names resolve against the primary target schema.
- Subscriptions are diffed on conninfo, enabled state, and publication set
  from `pg_subscription` (current database): missing → `CREATE SUBSCRIPTION`
  + `COMMENT ON SUBSCRIPTION … 'grizzle-managed'`, drift → `ALTER
  SUBSCRIPTION`, drop → gated by `AllowDropSubscription` +
  `DROP_SUBSCRIPTION` (CRITICAL). Default `DROP` keeps the remote slot
  (`DISABLE` + `SET (slot_name = NONE)` then `DROP`). Conninfo is redacted
  in Step.SQL / plan JSON / Hash like role passwords (`SUBSCRIPTION_CONNINFO`
  WARNING). Apply materializes CONNECTION from IR; redacted-only IR refuses apply.
- Standalone logical slots (`pg_replication_slots` where `database =
  current_database()` and `slot_type = 'logical'`): create missing desired
  slots; never auto-sweep live-only slots (COMMENT ON is unsupported);
  explicit `SELECT pg_drop_replication_slot` sets a dropped set gated by
  `AllowDropReplicationSlot` + `DROP_REPLICATION_SLOT` (CRITICAL). Slots
  owned by a managed subscription (`subslotname`) are not a second object.
  Active slots (`active_pid IS NOT NULL`) refuse drop.
- Event triggers are diffed on event, tag filter, function, and enabled
  state from `pg_event_trigger`: definition drift is DROP+CREATE
  (PostgreSQL has no in-place ALTER for event/tags/function);
  enabled-only drift renders `ALTER EVENT TRIGGER ENABLE/DISABLE`.
- Event-trigger functions must exist (managed routines from `SchemaSQL`
  or live objects); the sync aborts naming the missing function.
- Drops are narrow: publications, event triggers, and subscriptions stamped
  with the `grizzle-managed` catalog comment are considered for dropping
  (behind their AllowDrop* gates + CRITICAL hazards), including the
  DROP+CREATE recreate path used when definition drift cannot be expressed
  as `ALTER`. Operator-created objects without markers are never swept; a
  same-name unmanaged object that requires recreate fails closed at plan
  time. Slots use the explicit-drop policy above.
- `CREATE EVENT TRIGGER` DDL may require superuser; every plan step
  touching one emits `EVENT_TRIGGER_SUPERUSER` (WARNING). `FOR ALL
  TABLES` breadth emits `PUBLICATION_ALL_TABLES` (NOTICE).
- Catalog steps sort after roles and grants (subscriptions after
  publications, before event triggers); the original `SchemaSQL` and
  `CatalogSQL` strings each participate in `Plan.Hash()` when non-empty
  (conninfo redacted). Extracted statements are not hashed a second time.
  SQLite + non-empty `CatalogSQL` is rejected.

```go
type ApplyOpts struct {
    ExpectedHash  string
    AcceptHazards []HazardCode
    Backfill      BackfillFunc
}

type ExportFormat string

const (
    FormatSQL   ExportFormat = "sql"
    FormatGoose ExportFormat = "goose"
    FormatAtlas ExportFormat = "atlas"
)

type Artifact struct {
    Filename string
    Content  string
}
```

### 2.3 Plan, hazard analysis, and visualization

```go
type Plan struct {
    TargetSchema   string
    TargetSchemas  []string
    Steps          []Step
    Policy         DropPolicy
    IncludeTables  []string
    ExcludeTables  []string
    Renames        map[string]string
    ExpandContract bool
    SchemaSQL      string

    // Approval-sensitive: participates in Hash() when non-empty (desired
    // roles/grants side-channel).
    RolesSQL string

    // Approval-sensitive: participates in Hash() when non-empty (desired
    // publications/event-triggers/subscriptions/slots side-channel).
    CatalogSQL string

    // Approval-sensitive: participates in Hash() because it changes generated SQL.
    NonConcurrentIndexes bool

    // Operational timeouts persisted for direct Apply. Bounded by
    // ValidateExecutionFields; excluded from Hash(). Lock identity and
    // shadow schema names are never persisted — LockID is derived at apply
    // from trusted target identity, LockNamespace is an ApplyOpts runtime
    // option, and shadow schemas are generated ephemerally per run.
    LockTimeout      time.Duration
    StatementTimeout time.Duration
}

func (p *Plan) Hash() string // schema(s), scope, renames, expand, policy, NonConcurrentIndexes, SchemaSQL, RolesSQL, CatalogSQL, ordered steps
func (p *Plan) ValidateExecutionFields() error
func (p *Plan) Hazards() []Hazard
func (p *Plan) Additions() int
func (p *Plan) Modifications() int
func (p *Plan) Deletions() int
func (p *Plan) Blocked() int
func (p *Plan) Summary() (adds, alters, drops, blocked int)
func (p *Plan) Format(w io.Writer, useColor bool) error
func (p *Plan) String() string

type HazardLevel string

const (
    HazardLevelCritical HazardLevel = "CRITICAL" // Data destruction (DROP TABLE/COLUMN, DROP_EXTENSION, DROP_POLICY, DROP_FUNCTION, DROP_TRIGGER, DROP_VIEW, DROP_DOMAIN, DROP_ROLE, REVOKE_PRIVILEGE, DROP_PUBLICATION, DROP_SUBSCRIPTION, DROP_REPLICATION_SLOT, DROP_EVENT_TRIGGER, TYPE_NARROW, RENAME_AMBIGUOUS, UNMANAGED_DEPENDENCY, GENERATED_REWRITE)
    HazardLevelWarning  HazardLevel = "WARNING"  // Execution or lockout risk (EXTENSION_PRIVILEGE, GRANT_PUBLIC, EVENT_TRIGGER_SUPERUSER, SUBSCRIPTION_CONNINFO, RLS_ENABLE, SECURITY_DEFINER, PARTITION_ATTACH_SCAN, PARTITION_PENDING_DETACH)
    HazardLevelNotice   HazardLevel = "NOTICE"   // Locking or performance impact (INDEX creation/drop, FK drop, COMMENT_CLEAR, PUBLICATION_ALL_TABLES)
)

// ValidateExecutionFields bounds persisted timeouts to [0, 24h].
// Lock identity and shadow schema names are not carried in the artifact.

type HazardCode string

const (
    HazardDropTable              HazardCode = "DROP_TABLE"
    HazardDropColumn             HazardCode = "DROP_COLUMN"
    HazardTypeNarrow             HazardCode = "TYPE_NARROW"
    HazardNotNullNoDefault       HazardCode = "NOT_NULL_NO_DEFAULT"
    HazardIndexBuild             HazardCode = "INDEX_BUILD"
    HazardDropIndex              HazardCode = "DROP_INDEX"
    HazardDropFK                 HazardCode = "DROP_FK"
    HazardDropCheck              HazardCode = "DROP_CHECK"
    HazardCheckValidateScan      HazardCode = "CHECK_VALIDATE_SCAN"
    HazardRenameAmbiguous        HazardCode = "RENAME_AMBIGUOUS"
    HazardUnmanagedDependency    HazardCode = "UNMANAGED_DEPENDENCY"
    HazardGeneratedRewrite       HazardCode = "GENERATED_REWRITE"
    HazardPartitionAttachScan    HazardCode = "PARTITION_ATTACH_SCAN"
    HazardPartitionPendingDetach HazardCode = "PARTITION_PENDING_DETACH"
    HazardExtensionPrivilege     HazardCode = "EXTENSION_PRIVILEGE"
    HazardDropExtension          HazardCode = "DROP_EXTENSION"
    HazardDropPolicy             HazardCode = "DROP_POLICY"
    HazardRLSEnable              HazardCode = "RLS_ENABLE"
    HazardDropFunction           HazardCode = "DROP_FUNCTION"
    HazardSecurityDefiner        HazardCode = "SECURITY_DEFINER"
    HazardDropTrigger            HazardCode = "DROP_TRIGGER"
    HazardDropView               HazardCode = "DROP_VIEW"
    HazardDropDomain             HazardCode = "DROP_DOMAIN"
    HazardRevokePrivilege        HazardCode = "REVOKE_PRIVILEGE"
    HazardDropRole               HazardCode = "DROP_ROLE"
    HazardGrantPublic            HazardCode = "GRANT_PUBLIC"
    HazardPasswordChange         HazardCode = "PASSWORD_CHANGE"
    HazardDropPublication        HazardCode = "DROP_PUBLICATION"
    HazardDropSubscription       HazardCode = "DROP_SUBSCRIPTION"
    HazardDropReplicationSlot    HazardCode = "DROP_REPLICATION_SLOT"
    HazardSubscriptionConnInfo   HazardCode = "SUBSCRIPTION_CONNINFO"
    HazardDropEventTrigger       HazardCode = "DROP_EVENT_TRIGGER"
    HazardEventTriggerSuperuser  HazardCode = "EVENT_TRIGGER_SUPERUSER"
    HazardPublicationAllTables   HazardCode = "PUBLICATION_ALL_TABLES"
)

type Hazard struct {
    Code        HazardCode  `json:"code"`
    Level       HazardLevel `json:"level"`
    Type        ChangeType  `json:"type"`
    Table       string      `json:"table"`
    Description string      `json:"description"`
    SQL         string      `json:"sql"`
}
```

## 3. Supported schema constructs

### PostgreSQL

| Construct | Supported | Notes |
| :--- | :---: | :--- |
| `CREATE TABLE` | Yes | Composite primary keys, unlogged tables |
| `DROP TABLE` | Yes | Guarded by `AllowDropTable` and `HazardDropTable` |
| `ADD COLUMN` | Yes | Default expressions, nullability, generated columns |
| `DROP COLUMN` | Yes | Guarded by `AllowDropColumn` and `HazardDropColumn` |
| `RENAME COLUMN` | Yes | Atomic rename via `ALTER TABLE ... RENAME COLUMN` or staged expand |
| `ALTER COLUMN TYPE` | Yes | Automatic `USING` cast; guarded by `HazardTypeNarrow` if narrowed |
| `ALTER COLUMN SET/DROP NOT NULL` | Yes | Guarded by `HazardNotNullNoDefault` if non-null without default |
| `ALTER COLUMN SET/DROP DEFAULT` | Yes | Literal, function, and interval defaults |
| `CREATE INDEX` | Yes | Emitted as `CONCURRENTLY` by default; B-tree, GIN, GiST, BRIN, unique, partial |
| `DROP INDEX` | Yes | Guarded by `AllowDropIndex` and `HazardDropIndex` |
| `FOREIGN KEY` | Yes | Split into `ADD CONSTRAINT ... NOT VALID` and `VALIDATE CONSTRAINT` |
| `CHECK Constraint` | Yes | Named and inline `CHECK`; staged `ADD ... NOT VALID` then `VALIDATE CONSTRAINT`; drops guarded by `AllowDropCheck` |
| `ENUM Types` | Yes | `CREATE TYPE ... AS ENUM`, `ALTER TYPE ... ADD VALUE` |
| `Generated Columns` | Yes | `GENERATED ALWAYS AS (...) STORED`; expression rewrite triggers `GENERATED_REWRITE` hazard |
| `Native Types` | Yes | UUID, JSONB, Arrays, Timestamps, Numerics |
| `Extensions` | Yes | Declarative `CREATE EXTENSION` (desired captured by statement scan; installs best-effort in the shadow tx so extension-provided types compile). Creates are never dropped by default (`AllowDropExtension`) and marked irreversible in exports. `EXTENSION_PRIVILEGE` (WARNING) on create |
| `Row-Level Security` | Yes | `ENABLE/DISABLE/FORCE ROW LEVEL SECURITY` flags diffed per table (`pg_class`); `RLS_ENABLE` (WARNING) because applying can lock out the app role |
| `RLS Policies` | Yes | Full lifecycle via `pg_policy` (`CREATE POLICY` / DROP+CREATE replace); drops gated by `AllowDropPolicy` + `DROP_POLICY` (CRITICAL) |
| `COMMENT ON` | Yes | Table/column comments diffed from `obj_description` / `col_description`; set/clear (`IS '…'` / `IS NULL`) is non-destructive; clearing a non-empty live comment emits `COMMENT_CLEAR` (NOTICE); SQLite ignores comments (no catalog support) |
| `Functions` | Yes | Managed routines diffed on canonical `pg_get_functiondef` output; body drift → `CREATE OR REPLACE FUNCTION`, signature drift → DROP+CREATE; drops gated by `AllowDropFunction` + `DROP_FUNCTION` (CRITICAL); `SECURITY_DEFINER` (WARNING) without explicit `search_path` |
| `Procedures` | Yes | Managed like functions (`pg_get_functiondef` is canonical for both); body drift → `CREATE OR REPLACE PROCEDURE`; signature drift → DROP+CREATE; drops share the `AllowDropFunction` gate |
| `Aggregates` | Yes | Reconstructed canonical `CREATE AGGREGATE` from `pg_aggregate` (`SFUNC`/`STYPE`/`FINALFUNC`/`INITCOND`/`PARALLEL`); any drift is DROP+CREATE (no `CREATE OR REPLACE`); ordered-set/hypothetical aggregates (non-default `aggkind`) remain protected |
| `Domains` | Yes | Managed types diffed from `pg_type` (`typtype='d'`) + `pg_constraint` (`conrelid = 0`); base type/nullability/default drift is DROP+CREATE (no in-place retype); CHECK drift → `ALTER DOMAIN ADD/DROP CONSTRAINT`; drops gated by `AllowDropDomain` + `DROP_DOMAIN` (CRITICAL); no implicit `CASCADE` — dependent columns fail at apply |
| `Triggers` | Yes | `pg_trigger` + canonical `pg_get_triggerdef`; drift → DROP+CREATE; drops gated by `AllowDropTrigger` + `DROP_TRIGGER` (CRITICAL); surviving managed triggers block dependent column drops via `UNMANAGED_DEPENDENCY` |
| `Views` & `Materialized Views` | Yes | Canonical `pg_get_viewdef`; append-only column changes replace in place (`CREATE OR REPLACE VIEW`), otherwise DROP+CREATE; matviews always DROP+CREATE plus `REFRESH MATERIALIZED VIEW`; drops gated by `AllowDropView` + `DROP_VIEW` (CRITICAL) |
| `Roles & Grants` | Yes | Via unified `SchemaSQL` or the `RolesSQL` side-channel overlay (§2.2): managed roles (LOGIN/PASSWORD/config attrs) and object grants diffed against `pg_authid` + `pg_db_role_setting` + ACLs (`aclexplode`); duplicate side-channel identities override SchemaSQL; missing → `GRANT`, surplus → `REVOKE` gated by `AllowRevoke` + `REVOKE_PRIVILEGE` (CRITICAL); marker-stamped roles dropped behind `AllowDropRole` + `DROP_ROLE` (CRITICAL) with ownership refusal; grants to `PUBLIC`/unmanaged grantees never revoked; `GRANT ... TO PUBLIC` emits `GRANT_PUBLIC` (WARNING); password drift emits `PASSWORD_CHANGE` (WARNING); `SUPERUSER`/`REPLICATION`/`BYPASSRLS` refused |
| `Publications` | Yes | Via unified `SchemaSQL` or the `CatalogSQL` side-channel overlay (§2.2): membership and `publish` flags diffed from `pg_publication` / `pg_publication_rel` / `pg_publication_namespace`; duplicate side-channel names override SchemaSQL; drift → `ALTER PUBLICATION`; drops gated by `AllowDropPublication` + `DROP_PUBLICATION` (CRITICAL), narrow to marker-stamped objects; `FOR ALL TABLES` emits `PUBLICATION_ALL_TABLES` (NOTICE); schema-level publications require PostgreSQL 15+ |
| `Subscriptions` | Yes | Via unified `SchemaSQL` or `CatalogSQL`: conninfo/enabled/publication-set diffed from `pg_subscription` (current DB); CREATE stamps `COMMENT ON SUBSCRIPTION … 'grizzle-managed'`; DROP keeps remote slot (`DISABLE` + `SET (slot_name = NONE)`); gated by `AllowDropSubscription` + `DROP_SUBSCRIPTION` (CRITICAL); conninfo redacted (`SUBSCRIPTION_CONNINFO` WARNING); unsupported WITH options refused |
| `Logical replication slots` | Yes | Standalone `SELECT pg_create_logical_replication_slot` / `pg_drop_replication_slot`; live-only never swept (no COMMENT ON); subscription-owned slots ignored; active slots refused; gated by `AllowDropReplicationSlot` + `DROP_REPLICATION_SLOT` (CRITICAL); physical slots refused |
| `Event Triggers` | Yes | Via unified `SchemaSQL` or the `CatalogSQL` side-channel overlay (§2.2): event/tag/function/enabled state diffed from `pg_event_trigger`; duplicate side-channel names override SchemaSQL; definition drift is DROP+CREATE, enabled-only drift renders `ALTER EVENT TRIGGER ENABLE/DISABLE`; creation refuses missing trigger functions; drops gated by `AllowDropEventTrigger` + `DROP_EVENT_TRIGGER` (CRITICAL), narrow to marker-stamped objects; `EVENT_TRIGGER_SUPERUSER` (WARNING) on any event-trigger step |

### SQLite

| Construct | Supported | Notes |
| :--- | :---: | :--- |
| `CREATE TABLE` | Yes | Primary keys, autoincrement, column constraints |
| `DROP TABLE` | Yes | Guarded by `AllowDropTable` |
| `ADD COLUMN` | Yes | Direct `ALTER TABLE ... ADD COLUMN` when constraints permit |
| `RENAME COLUMN` | Yes | Direct `ALTER TABLE ... RENAME COLUMN` when mapped explicitly |
| `DROP COLUMN` | Yes | Handled via SQLite 12-step table rebuild |
| `ALTER COLUMN TYPE` | Yes | Handled via SQLite 12-step table rebuild |
| `CREATE INDEX` | Yes | Direct `CREATE INDEX` and `CREATE UNIQUE INDEX` |
| `DROP INDEX` | Yes | Guarded by `AllowDropIndex` |
| `FOREIGN KEY` | Yes | Validated with `PRAGMA foreign_key_check` |
| `CHECK Constraint` | Yes | Named and inline CHECK constraints are parsed from `sqlite_schema.sql`, emitted as table-level `CONSTRAINT` lines on create/rebuild, and diffed: check drift triggers a 12-step table rebuild; removals are destructive and gated by `AllowDropCheck` + `DROP_CHECK` (no `NOT VALID`/`VALIDATE` path on SQLite) |
| `Generated Columns` | Yes | `STORED` and `VIRTUAL` supported; rebuild preserves generated definitions |

### Unmanaged database objects (Detected, Protected, Not Managed)

| Construct | Supported | Policy |
| :--- | :---: | :--- |
| `Sequences` (unowned) | Protected | Never dropped or managed |
| `Window functions` (prokind `w`) | Protected | Never managed; dependency-graphed so drops referencing them stay blocked |
| Ordered-set / hypothetical aggregates (non-default `aggkind`) | Protected | Outside the reconstructed `CREATE AGGREGATE` surface; never managed |

Everything outside the tables above that Grizzle cannot fully diff is left untouched. Extension-owned objects (e.g. types installed by `citext`) are excluded from routine management (`pg_proc.deptype = 'e'`).

## 4. Safety model and invariants

### Invariant 1: Non-destructive by default
If `AllowDrop` is false (the default), Grizzle refuses to execute any plan containing table or column deletions. It returns a `*DestructiveViolationError` detailing the blocked operations.

### Invariant 2: Distributed mutual exclusion with post-lock re-diffing
PostgreSQL migrations acquire advisory locks (`pg_advisory_xact_lock` or session lock for concurrent indexes). Grizzle recomputes the diff post-lock to avoid TOCTOU races.

### Invariant 3: Hazard gating
Critical hazards (`DROP_TABLE`, `DROP_COLUMN`, `TYPE_NARROW`, `RENAME_AMBIGUOUS`, `UNMANAGED_DEPENDENCY`, `GENERATED_REWRITE`, `DROP_ROLE`, `REVOKE_PRIVILEGE`, `DROP_PUBLICATION`, `DROP_SUBSCRIPTION`, `DROP_REPLICATION_SLOT`, `DROP_EVENT_TRIGGER`) fail execution unless accepted via `AcceptHazards`.

### Invariant 4: Plan/Apply approval hash
`Plan.Hash()` digests approval-sensitive intent (target schemas, scope, renames, expand/contract, policy, NonConcurrentIndexes, SchemaSQL, RolesSQL, CatalogSQL, ordered steps). `Apply` / `DryRunVerifyPlan` verify against `ExpectedHash`, aborting with `ErrPlanDrift` on mismatch. History is advisory (Model B): DDL commits first; a failed history write returns `ErrHistoryRecord` while leaving schema changes applied.

### Invariant 5: Timeouts and retry
`LockTimeout` and `StatementTimeout` protect production availability. `LockTimeout` bounds the **total** advisory-lock acquisition wait across all retry attempts (including backoff); preamble work (hooks, schema setup, session timeout statements) and DDL execution do not consume the budget — each attempt's acquisition timer is armed when acquisition begins. PostgreSQL lock-contention and deadlock failures (SQLSTATE `55P03`, `40P01`) are classified via `exec.IsRetryable` and retried with exponential backoff and randomized jitter; retries halt immediately once any DDL step commits.

### Invariant 6: Strict scope protection
`StrictScope` ensures `IncludeTables` is provided, preventing accidental mutations in shared databases.

### Invariant 7: Expand and contract
Ambiguous column renames are blocked with `RENAME_AMBIGUOUS`; map them explicitly via `Options.Renames` (CLI: repeatable `--rename old=new`, optionally table-qualified `table.old=new`). Explicit renames execute either as atomic renames or, with `Options.ExpandContract` enabled (CLI: `--expand-contract`), as staged zero-downtime migrations:

1. **Expand plan**: renamed/modified columns are added alongside existing columns as nullable; nothing is dropped or rewritten.
2. **Backfill**: values are copied in batches outside the DDL lock window via the library-only `Options.Backfill` hook. There is no CLI backfill runner.
3. **Contract plan**: a second, separately-approved plan (own deterministic hash) drops the legacy columns once the application no longer reads them; destructive, so it requires `AllowDropColumn` and explicit `AcceptHazards`.

The `RENAME_AMBIGUOUS` hazard description and the interactive summary remediation point to `--rename`. See `examples/expand-contract` for a runnable end-to-end flow.

### Invariant 8: History and drift detection
Applied plans are audited in `grizzle_history`. `Check()` provides read-only schema drift verification. A failed success-path history write does not fail the applied migration: it is logged at ERROR and surfaced as a typed non-fatal error (`errors.Is(err, grizzle.ErrHistoryRecord)`); failure-path history write failures are logged and never swallowed.

## 5. Error taxonomy

```go
var (
    ErrEmptySchema        = errors.New("grizzle: schema SQL cannot be empty")
    ErrDestructiveBlocked = errors.New("grizzle: destructive change rejected by policy")
    ErrLockAcquisition    = errors.New("grizzle: failed to acquire advisory lock")
    ErrCompilationFailed  = errors.New("grizzle: schema compilation in shadow schema failed")
    ErrExecutionFailed    = errors.New("grizzle: applying DDL to live schema failed")
    ErrInspectionFailed   = errors.New("grizzle: inspecting schema failed")
    ErrUnsupportedDialect = errors.New("grizzle: unsupported database dialect")
    ErrHazardBlocked      = errors.New("grizzle: migration blocked by unaccepted critical hazards")
    ErrPlanDrift          = errors.New("grizzle: plan drifted from approved state")
    ErrDrift              = errors.New("grizzle: live database schema has drifted from desired schema")
    ErrLockTimeout             = errors.New("grizzle: lock acquisition timed out")
    ErrInvalidOptions          = errors.New("grizzle: invalid options")
    ErrStrictScope             = errors.New("grizzle: strict scope requires non-empty IncludeTables")
    ErrPartitionConversion     = errors.New("grizzle: in-place conversion between regular and partitioned table is unsupported")
    ErrUnsupportedMultiSchema  = errors.New("grizzle: multi-schema CompileSchema is unsupported for PostgreSQL")
    ErrPartitionKeyNotInUnique = errors.New("grizzle: primary key or unique constraint must include all partition key columns")
    ErrHistoryRecord           = errors.New("grizzle: migration succeeded but history record was not written") // non-fatal
)

type HazardError struct {
    Hazards []Hazard
}

type DriftError struct {
    Plan *Plan
}

type DestructiveViolationError struct {
    Violations []Step
}
```

## 6. Schema linting

`LintSchema` statically checks the desired schema IR. The lint layer is pure: it imports only `internal/schema`, performs no I/O, and produces identical diagnostics for identical input DDL. `DefaultLintRules()` returns the built-in rule set:

| Rule | Severity | Check |
| :--- | :--- | :--- |
| `L001` | `ERROR` | Table has no primary key (partitions are skipped; the parent's key covers them) |
| `L002` | `WARNING` | Foreign key columns are not a prefix of any index (including the primary key) |
| `L003` | `WARNING` | Table, column, index, or enum name is not lowercase snake_case |
| `L004` | `WARNING` | Column uses a legacy `SERIAL` / `nextval` default instead of `GENERATED ALWAYS AS IDENTITY` |
| `L005` | `WARNING` | CHECK constraint name is not lowercase snake_case |
| `L006` | `WARNING` | Table declares multiple CHECK constraints with the same normalized expression |
| `L007` | `INFO` | CHECK constraint uses PostgreSQL's auto-generated name (`table[_column]_check[n]`); prefer an explicit `CONSTRAINT name CHECK` |
| `L008` | `WARNING` | Table has `ENABLE ROW LEVEL SECURITY` but declares zero policies — every role is locked out |
| `L009` | `ERROR` | DML statement (`INSERT`/`UPDATE`/`DELETE`/`TRUNCATE`) declared in `SchemaSQL` — it is silently ignored by automigrations; move seed data to `SeedSQL` |

Severity semantics:

- `ERROR`: structural anti-pattern; `LintHasErrors` reports it and release gates should fail.
- `WARNING`: recommendation that may be ignored deliberately.
- `INFO`: stylistic suggestion with no correctness impact.

Custom rules implement the `LintRule` interface (`ID`, `Description`, `Check(*SchemaIR) []LintDiagnostic`) and may be mixed with `DefaultLintRules()`. Formatters: `LintFormatText` (human-readable with severity counts), `LintFormatJSON` (stable JSON array), `LintFormatGitHub` (workflow commands: `ERROR` → `::error`, `WARNING` → `::warning`, `INFO` → `::notice`).

```go
type LintDiagnostic struct {
    RuleID   string       `json:"rule_id"`
    Severity LintSeverity `json:"severity"`
    Table    string       `json:"table"`
    Column   string       `json:"column,omitempty"`
    Message  string       `json:"message"`
    Line     int          `json:"line,omitempty"`
}

type LintRule interface {
    ID() string
    Description() string
    Check(s *SchemaIR) []LintDiagnostic
}

func LintSchema(s *SchemaIR, rules ...LintRule) []LintDiagnostic
func DefaultLintRules() []LintRule
func LintHasErrors(diags []LintDiagnostic) bool
func LintFormatText(w io.Writer, diags []LintDiagnostic) error
func LintFormatJSON(w io.Writer, diags []LintDiagnostic) error
func LintFormatGitHub(w io.Writer, diags []LintDiagnostic) error
```
