# Grizzle SQL Review & Linter Specification

> **A Comprehensive Catalog of Declarative Schema Linting & SQL Review Rules**  
> *Derived from PostgreSQL Production Best Practices, Database Reliability Engineering Standards, and Grizzle Safety Invariants.*

---

## 1. Architectural Philosophy

Grizzle's linter provides **static, database-free, deterministic inspection** of declarative database schemas.

```
schema.sql (Source DDL)
         │
         ▼ (Shadow Compilation / Parser)
  *schema.Schema (IR) ──────────┐
         │                      │
         ▼                      ▼
  Grizzle Linter          Grizzle Diff Engine
  (Static Rules: L001..L080)  (Dynamic Hazards: Plan.Hazards())
         │                      │
         ▼                      ▼
  Diagnostics (PR Lint)   Gated Execution Plan (Advisory Lock)
```

### The Grizzle Advantage: Linter Rules vs. Dynamic Hazards
Traditional imperative SQL review tools lint raw migration scripts and flag warnings (e.g. *"Warning: You forgot `CONCURRENTLY` on CREATE INDEX"*).  
**Grizzle goes a step further**:
1. **Static Linter Rules (`L001`–`L080`)**: Inspect the desired declarative `schema.sql` AST *before* touching any database. They enforce naming conventions, architectural standards, type constraints, and security policies.
2. **Dynamic Plan Hazards (`HazardCode`)**: Grizzle's diff engine **automatically fixes** operational DDL traps (e.g., automatically generates `CREATE INDEX CONCURRENTLY` and `NOT VALID` foreign keys) and gates destructive operations (`DROP_TABLE`, `TYPE_NARROW`) behind explicit cryptographic approvals.

### Severity Semantics
* 🔴 **`ERROR`**: Structural anti-pattern or severe correctness/data-loss bug. Must be resolved before migration can proceed.
* 🟡 **`WARNING`**: Operational, performance, or locking risk. Should be reviewed and acknowledged by a tech lead.
* 🟢 **`INFO`**: Stylistic or convention suggestion with zero operational risk.

### Suppression Syntax
Rules can be suppressed per-statement or per-file using SQL comments:
```sql
-- grizzle:lint-ignore L001, L003
CREATE TABLE legacy_session_store (
    session_token TEXT
);

-- grizzle:lint-disable L003
-- (Disables snake_case check for the remainder of this file)
```

---

## 2. Currently Shipped Built-In Rules (`L001`–`L013`)

These rules are currently implemented in Grizzle's [`internal/lint`](../internal/lint):

| Rule ID | Severity | Name | Description |
| :--- | :--- | :--- | :--- |
| `L001` | `ERROR` | **MissingPrimaryKey** | Table has no primary key. Required for logical replication (`REPLICA IDENTITY`) and unique row identification. |
| `L002` | `WARNING` | **UnindexedForeignKey** | Foreign key column set is not covered by any index prefix. Causes sequential scans on parent updates/deletes. |
| `L003` | `WARNING` | **NamingConvention** | Table, column, index, or enum name is not lowercase `snake_case` (`^[a-z][a-z0-9_]*$`). |
| `L004` | `WARNING` | **PreferIdentityOverSerial** | Column uses legacy `SERIAL` / `nextval` default instead of SQL-standard `GENERATED ALWAYS AS IDENTITY`. |
| `L005` | `WARNING` | **CheckNamingConvention** | `CHECK` constraint name is not lowercase `snake_case`. |
| `L006` | `WARNING` | **DuplicateCheckConstraint** | Table declares multiple `CHECK` constraints with identical normalized expressions. |
| `L007` | `INFO` | **PreferNamedChecks** | `CHECK` constraint uses an auto-generated PostgreSQL name (`table[_col]_check[n]`); prefer an explicit constraint name. |
| `L008` | `WARNING` | **RLSEnableWithoutPolicies** | Table has `ENABLE ROW LEVEL SECURITY` but declares zero policies — all non-superuser roles are locked out. |
| `L009` | `ERROR` | **NoDMLStatements** | DML statement (`INSERT`/`UPDATE`/`DELETE`/`TRUNCATE`) declared in `SchemaSQL`; seeds must live in `SeedSQL`. |
| `L010` | `ERROR` | **TriggerWithoutFunction** | Trigger references a routine that is neither declared in `SchemaSQL` nor present in catalog. |
| `L011` | `WARNING` | **UnversionedExtension** | `CREATE EXTENSION` does not specify an explicit `VERSION 'x.y'`. |
| `L012` | `ERROR` | **NonIdempotentSeed** | Seed DML in `SeedSQL` lacks an `ON CONFLICT` clause or idempotent guard. |
| `L013` | `ERROR` | **UnsupportedSQLiteFeature** | Schema attempts to use features unsupported on SQLite (e.g. `COMMENT ON`, RLS, or enum types). |

### Strict Profile Rules (`--profile strict`)
| Rule ID | Severity | Name | Description |
| :--- | :--- | :--- | :--- |
| `L014` | `ERROR` | **PrimaryKeyTypeAllowlist** | Primary key columns must use an allowed integer or UUID data type (`BIGINT`, `INTEGER`, `SMALLINT`, `UUID`). |
| `L022` | `ERROR` | **DisallowReservedKeyword** | Tables and columns must not use reserved SQL keywords (`user`, `order`, `table`, etc.). |
| `L044` | `WARNING` | **RequireBooleanDefault** | Boolean columns should declare an explicit `DEFAULT false` or `DEFAULT true` to avoid tri-state `NULL`. |
| `L047` | `ERROR` | **DisallowDuplicateIndex** | Table must not declare duplicate indexes covering the identical columns, access method, and predicate. |
| `L048` | `WARNING` | **DisallowRedundantPrefixIndex** | Single-column index is redundant when a composite index covers the column as its leading prefix. |

---

## 3. Exhaustive Rule Catalog (`L014`–`L080`)

*The following rules establish comprehensive industry-standard SQL review rules adapted specifically for declarative PostgreSQL & SQLite schemas.*

### Category 1: Table & Primary Key Design (`L014`–`L022`)

| Rule ID | Severity | Rule Name | Reference Standard | Description & Recommendation |
| :--- | :--- | :--- | :--- | :--- |
| `L014` | `ERROR` | **PrimaryKeyTypeAllowlist** | `PK_TYPE_ALLOWLIST` | Primary key data type must be in the approved list (`BIGINT`, `INT8`, `UUID`). Disallows `TEXT`, `VARCHAR`, or floating-point PKs. |
| `L015` | `WARNING` | **DisallowCompositePrimaryKey** | `PK_KEY_NUMBER_LIMIT` | Recommends surrogate single-column primary keys. Tables with >2 columns in PK should use a unique constraint instead. |
| `L016` | `INFO` | **RequireTableComment** | `TABLE_COMMENT_CONVENTION` | Every table must declare an informative `COMMENT ON TABLE "tbl" IS '...'` for data cataloging. |
| `L017` | `WARNING` | **RequireAuditColumns** | `AUDIT_COLUMNS_REQUIRED` | Tables must include standard audit timestamps: `created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`. |
| `L018` | `ERROR` | **DisallowUnqualifiedSchema** | `EXPLICIT_TARGET_SCHEMA` | Objects must be created within an explicit target schema rather than falling back to default ambient `public`. |
| `L019` | `WARNING` | **DisallowTablePartition** | `DISALLOW_PARTITION` | Restricts declarative table partitioning unless the project explicitly opts into partitioned storage. |
| `L020` | `ERROR` | **DisallowTableDropCascade** | `DISALLOW_CASCADE_DROP` | `DROP TABLE ... CASCADE` is forbidden; dependent objects must be explicitly removed in topological order. |
| `L021` | `WARNING` | **TableNameLengthLimit** | `TABLE_NAME_LENGTH` | Table names must not exceed 63 characters (PostgreSQL identifier truncation limit). |
| `L022` | `ERROR` | **DisallowReservedKeywordTableName** | `KEYWORD_IDENTIFIER` | Table name must not collide with SQL reserved keywords (`user`, `order`, `group`, `table`, `select`). |

---

### Category 2: Foreign Keys & Relational Integrity (`L023`–`L030`)

| Rule ID | Severity | Rule Name | Reference Standard | Description & Recommendation |
| :--- | :--- | :--- | :--- | :--- |
| `L023` | `WARNING` | **DisallowCascadeDeleteOnHighVolume** | `DISALLOW_CASCADE_DELETE` | `ON DELETE CASCADE` causes uncontrolled recursive row deletions under high write concurrency; prefer soft deletes. |
| `L024` | `ERROR` | **ForeignKeyTypeMismatch** | `FK_TYPE_MISMATCH` | Foreign key column type must strictly match the referenced primary key type (e.g. `BIGINT` referencing `BIGINT`, not `INT`). |
| `L025` | `INFO` | **RequireForeignKeyName** | `REQUIRE_CONSTRAINT_NAME` | Foreign keys must declare an explicit `CONSTRAINT fk_...` rather than relying on anonymous constraints. |
| `L026` | `WARNING` | **ForeignKeyNamingConvention** | `NAMING_FK_CONVENTION` | Foreign key constraint names must follow the template `fk_{table}_{referenced_table}` or `fk_{table}_{column}`. |
| `L027` | `ERROR` | **DisallowCrossDatabaseForeignKey** | `DISALLOW_CROSS_DB_FK` | Foreign keys cannot reference tables in other databases or unmanaged external schemas. |
| `L028` | `WARNING` | **DisallowForeignKeysForShardedTables** | `DISALLOW_SHARDED_FK` | Disallows foreign keys for architectures targeting distributed / sharded databases (e.g. Citus, Vitess). |
| `L029` | `ERROR` | **ForeignKeyTargetUnique** | `FK_TARGET_NOT_UNIQUE` | The referenced column in the parent table must be covered by a `PRIMARY KEY` or `UNIQUE` constraint. |
| `L030` | `WARNING` | **SelfReferencingForeignKeyNullable** | `SELF_REF_FK_NULLABLE` | Self-referencing hierarchical foreign keys (`parent_id REFERENCES tbl(id)`) must be nullable to allow root nodes. |

---

### Category 3: Column Specifications & Data Types (`L031`–`L044`)

| Rule ID | Severity | Rule Name | Reference Standard | Description & Recommendation |
| :--- | :--- | :--- | :--- | :--- |
| `L031` | `WARNING` | **DisallowUnboundedVarchar** | `VARCHAR_LENGTH_LIMIT` | Flag `VARCHAR` without an explicit length limit; prefer typed `VARCHAR(n)` or deliberate `TEXT`. |
| `L032` | `ERROR` | **DisallowFloatForFinancialData** | `DISALLOW_FLOAT_FINANCIAL` | Disallows IEEE 754 floating-point types (`REAL`, `FLOAT4`, `FLOAT8`) for money columns; enforce `NUMERIC` / `DECIMAL`. |
| `L033` | `WARNING` | **DisallowLargeBlobInOLTP** | `DISALLOW_INLINE_BLOB` | Flags large inline `BYTEA` or binary blobs in OLTP tables; recommend S3/R2 storage with URL references. |
| `L034` | `ERROR` | **DisallowDangerousTypeNarrowing** | `DANGEROUS_TYPE_NARROWING` | Narrowing a column type (e.g. `BIGINT` to `INT`, `VARCHAR(100)` to `VARCHAR(50)`) risks immediate runtime truncation failure. |
| `L035` | `INFO` | **RequireColumnComment** | `REQUIRE_COLUMN_COMMENT` | Sensitive or non-obvious columns should declare `COMMENT ON COLUMN "tbl"."col" IS '...'`. |
| `L036` | `ERROR` | **DisallowVolatileDefaultValue** | `DISALLOW_VOLATILE_DEFAULT` | Column defaults must be deterministic; disallow volatile functions like `random()` or non-stable expressions. |
| `L037` | `WARNING` | **DisallowNullableWithoutDefault** | `REQUIRE_DEFAULT_IF_NULLABLE` | Recommends that nullable non-foreign-key columns either specify a default or be marked `NOT NULL`. |
| `L038` | `WARNING` | **PreferJsonbOverJson** | `PREFER_JSONB_OVER_JSON` | In PostgreSQL, prefer `JSONB` over legacy `JSON` for indexed query performance and binary storage efficiency. |
| `L039` | `WARNING` | **ColumnNameLengthLimit** | `COLUMN_NAME_LENGTH` | Column names must not exceed 63 characters. |
| `L040` | `ERROR` | **DisallowReservedKeywordColumnName** | `KEYWORD_COLUMN_NAME` | Column names must not collide with SQL keywords (`key`, `value`, `from`, `desc`, `all`). |
| `L041` | `WARNING` | **TimestampMustHaveTimezone** | `REQUIRE_TIMESTAMPTZ` | Disallows naive `TIMESTAMP WITHOUT TIME ZONE`; all temporal fields must use `TIMESTAMPTZ`. |
| `L042` | `WARNING` | **TableColumnCountLimit** | `TABLE_COLUMN_COUNT_LIMIT` | Warns when a table exceeds 50 columns. Deeply nested tables degrade row unpacking performance. |
| `L043` | `WARNING` | **TotalVarcharLengthLimit** | `TOTAL_VARCHAR_LIMIT` | Sum of max length of all `VARCHAR` columns in a table should not exceed 8KB (PostgreSQL page size limit). |
| `L044` | `WARNING` | **RequireBooleanDefault** | `REQUIRE_BOOLEAN_DEFAULT` | Boolean columns must declare `DEFAULT false` or `DEFAULT true` to eliminate tri-state Boolean bugs (`NULL`). |

---

### Category 4: Index Hygiene & Performance (`L045`–`L055`)

| Rule ID | Severity | Rule Name | Reference Standard | Description & Recommendation |
| :--- | :--- | :--- | :--- | :--- |
| `L045` | `WARNING` | **TableIndexCountLimit** | `TABLE_INDEX_COUNT_LIMIT` | Warns when a table has more than 5 secondary indexes; excessive indexes degrade write throughput. |
| `L046` | `WARNING` | **IndexColumnNumberLimit** | `INDEX_COLUMN_LIMIT` | Composite indexes should not exceed 3–4 columns. Long composite indexes bloat the b-tree. |
| `L047` | `ERROR` | **DisallowDuplicateIndex** | `DISALLOW_DUPLICATE_INDEX` | Disallow declaring two indexes on the same table covering the exact same columns and access method. |
| `L048` | `WARNING` | **DisallowRedundantPrefixIndex** | `DISALLOW_REDUNDANT_INDEX` | An index on `(a, b)` already accelerates queries on `(a)`; a standalone index on `(a)` is redundant. |
| `L049` | `WARNING` | **IndexNamingConvention** | `NAMING_INDEX_CONVENTION` | Secondary index names must follow `idx_{table}_{column(s)}`. |
| `L050` | `WARNING` | **UniqueIndexNamingConvention** | `NAMING_UNIQUE_CONVENTION` | Unique constraint/index names must follow `uk_{table}_{column(s)}` or `uniq_{table}_{column(s)}`. |
| `L051` | `WARNING` | **DisallowBtreeOnComplexTypes** | `DISALLOW_BTREE_COMPLEX` | Standard B-Tree indexes on `JSONB`, arrays, or geometric types are inefficient; recommend `GIN` or `GiST`. |
| `L052` | `INFO` | **RecommendPartialIndexOnSoftDelete** | `PARTIAL_INDEX_SOFT_DELETE` | For tables with `deleted_at`, unique constraints should be partial: `WHERE deleted_at IS NULL`. |
| `L053` | `WARNING` | **DisallowLowCardinalityIndex** | `DISALLOW_LOW_CARDINALITY_INDEX` | B-tree index on low-cardinality boolean/status columns will rarely be selected by query planner; use partial index. |
| `L054` | `WARNING` | **IndexNameLengthLimit** | `INDEX_NAME_LENGTH` | Index names must not exceed 63 characters. |
| `L055` | `INFO` | **UniqueNullsNotDistinct** | `UNIQUE_NULLS_NOT_DISTINCT` | In PostgreSQL 15+, unique constraints over nullable columns should declare `NULLS NOT DISTINCT` if unique enforcement is intended. |

---

### Category 5: Concurrency, DDL & Migration Safety (`L056`–`L065`)

| Rule ID | Severity | Rule Name | Reference Standard | Description & Recommendation |
| :--- | :--- | :--- | :--- | :--- |
| `L056` | `ERROR` | **RequireConcurrentIndexCreation** | `CONCURRENT_INDEX_CREATION` | Index creation must use `CONCURRENTLY` to avoid taking `SHARE UPDATE EXCLUSIVE` locks on live tables. *(Grizzle diff engine handles this automatically)*. |
| `L057` | `ERROR` | **RequireStagedConstraintValidation** | `STAGED_CONSTRAINT_VALIDATION` | Foreign keys and check constraints must be added as `NOT VALID` followed by `VALIDATE CONSTRAINT`. *(Grizzle handles this automatically)*. |
| `L058` | `ERROR` | **DisallowNotNullOnExistingTable** | `DISALLOW_NOT_NULL_WITHOUT_DEFAULT` | Adding `NOT NULL` without a default value causes full-table scans and locks; requires staged backfill. |
| `L059` | `ERROR` | **DisallowDestructiveDropTable** | `DISALLOW_DROP_TABLE` | Dropping a table from `schema.sql` causes permanent data loss; requires explicit `AllowDropTable` gate. |
| `L060` | `ERROR` | **DisallowDestructiveDropColumn** | `DISALLOW_DROP_COLUMN` | Dropping a column causes permanent data loss; requires `AllowDropColumn` gate. |
| `L061` | `ERROR` | **DisallowTruncateInMigrations** | `DISALLOW_TRUNCATE` | `TRUNCATE` statements in migrations wipe operational data without transaction recovery. |
| `L062` | `WARNING` | **RequireExpandContractForRenames** | `EXPAND_CONTRACT_RENAME` | In-place column renames break rolling deployments; enforce the Expand/Contract phased workflow. |
| `L063` | `ERROR` | **DisallowDropSchemaCascade** | `DISALLOW_CASCADE_DROP_SCHEMA` | `DROP SCHEMA ... CASCADE` drops all enclosed objects indiscriminately; forbidden. |
| `L064` | `INFO` | **GroupAltersPerTable** | `MERGE_ALTER_TABLE` | Multiple `ALTER TABLE` statements on the same table should be merged into a single multi-clause statement to take the table lock once. |
| `L065` | `WARNING` | **RequireLockTimeoutAnnotation** | `LOCK_TIMEOUT_SPECIFIED` | DDL scripts should always declare a strict `lock_timeout = '5s'` to prevent queuing behind slow queries. |

---

### Category 6: Security, Roles & Permissions (`L066`–`L073`)

| Rule ID | Severity | Rule Name | Reference Standard | Description & Recommendation |
| :--- | :--- | :--- | :--- | :--- |
| `L066` | `ERROR` | **DisallowGrantAllToPublic** | `DISALLOW_PUBLIC_GRANT_ALL` | Granting broad privileges to `PUBLIC` allows ambient access to every connected database user. |
| `L067` | `WARNING` | **SecurityDefinerExplicitSearchPath** | `SECURITY_DEFINER_SEARCH_PATH` | `SECURITY DEFINER` routines must declare an explicit `SET search_path = pg_catalog, public` to prevent privilege escalation. |
| `L068` | `WARNING` | **RequireRLSOnMultiTenantTables** | `REQUIRE_RLS_MULTI_TENANT` | Tables with `tenant_id` or `org_id` columns should enable Row Level Security (`ENABLE ROW LEVEL SECURITY`). |
| `L069` | `ERROR` | **DisallowPlaintextPasswordInDDL** | `DISALLOW_PLAINTEXT_PASSWORD` | Passwords for database roles must never be committed in plaintext in SQL files. |
| `L070` | `ERROR` | **DisallowSuperuserGrantInAppSchema** | `DISALLOW_SUPERUSER_GRANT` | Application database roles must not be granted `SUPERUSER` privileges. |
| `L071` | `INFO` | **RequireExplicitTableOwner** | `REQUIRE_EXPLICIT_OWNER` | Tables should declare an explicit `ALTER TABLE ... OWNER TO app_user`. |
| `L072` | `WARNING` | **DisallowRevokeAllWithoutReplacement** | `DISALLOW_REVOKE_WITHOUT_REPLACEMENT` | Revoking all permissions from an active role locks out production services. |
| `L073` | `WARNING` | **DisallowTrivialRLSPolicy** | `DISALLOW_TRIVIAL_RLS` | Flags RLS policies with unconditional `USING (true)` in production environments. |

---

### Category 7: Routines, Triggers, DML & Seeds (`L074`–`L080`)

| Rule ID | Severity | Rule Name | Reference Standard | Description & Recommendation |
| :--- | :--- | :--- | :--- | :--- |
| `L074` | `INFO` | **DisallowStoredProcedures** | `DISALLOW_STORED_PROCEDURES` | Recommends placing business logic in application code rather than stored database procedures. |
| `L075` | `WARNING` | **RequireTriggerWhenFilter** | `REQUIRE_TRIGGER_WHEN` | Row-level triggers should declare a `WHEN (...)` clause to prevent executing procedural code for irrelevant row updates. |
| `L076` | `ERROR` | **DisallowCyclicViewDependency** | `DISALLOW_CYCLIC_VIEW` | View definitions must form a strict Directed Acyclic Graph (DAG). |
| `L077` | `ERROR` | **RequireExplicitSeedIdempotency** | `EXPLICIT_SEED_IDEMPOTENCY` | `INSERT` statements in `SeedSQL` must declare `ON CONFLICT DO NOTHING` or `DO UPDATE` to remain replayable. |
| `L078` | `ERROR` | **DisallowDMLWithoutWhere** | `DISALLOW_UNBOUNDED_DML` | `UPDATE` and `DELETE` statements in data migrations must have an explicit `WHERE` clause. |
| `L079` | `WARNING` | **DisallowSelectStarInViews** | `DISALLOW_SELECT_STAR_VIEWS` | Views must explicitly enumerate their projected columns; `SELECT *` creates silent schema drift when tables change. |
| `L080` | `ERROR` | **DisallowDropFunctionWithTrigger** | `DISALLOW_DROP_REFERENCED_ROUTINE` | Cannot drop a function that is actively referenced by a table trigger. |

---

## 4. Configuration Schema (`.grizzle.yaml`)

Users can customize severity, enable/disable rules, and configure threshold parameters via configuration:

```yaml
lint:
  # Global severity override: 'error' causes non-zero exit code
  fail_on: warning

  # Disable specific rules
  disabled_rules:
    - L016 # RequireTableComment
    - L031 # DisallowUnboundedVarchar

  # Rule-specific configuration parameters
  rules:
    L014: # PrimaryKeyTypeAllowlist
      severity: error
      allowed_types:
        - bigint
        - uuid

    L042: # TableColumnCountLimit
      severity: warning
      max_columns: 40

    L045: # TableIndexCountLimit
      severity: warning
      max_indexes: 6

    L046: # IndexColumnNumberLimit
      severity: warning
      max_columns: 3
```
