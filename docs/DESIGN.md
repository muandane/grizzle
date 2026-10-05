# Grizzle technical design

This document details the internal data structures, catalog queries, type normalization strategies, and diffing algorithms used by Grizzle.

## 1. Internal representation data structures

Grizzle normalizes live and desired database schemas into an engine-agnostic schema intermediate representation (`SchemaIR`):

```go
type SchemaIR struct {
    Name   string
    Tables map[string]*TableIR
    Enums  map[string]*EnumIR
}

type TableIR struct {
    Name        string
    Columns     map[string]*ColumnIR
    Indexes     map[string]*IndexIR
    ForeignKeys map[string]*ForeignKeyIR
    PrimaryKey  *PrimaryKeyIR
}

type ColumnIR struct {
    Name         string
    DataType     string // Normalized (e.g. "bigint", "varchar(255)", "boolean")
    IsNullable   bool
    DefaultValue string // Sanitized, stripped of Postgres casts or redundant parens
    Position     int
}

type IndexIR struct {
    Name       string
    TableName  string
    IsUnique   bool
    Definition string // Normalized DDL expression
}

type ForeignKeyIR struct {
    Name       string
    TableName  string
    Definition string // Normalized constraint definition
}

type PrimaryKeyIR struct {
    Name    string
    Columns []string
}

type EnumIR struct {
    Name   string
    Values []string
}
```

## 2. Catalog inspection

### 2.1 PostgreSQL catalog queries

Grizzle queries `pg_catalog` directly rather than `information_schema` for speed and full type fidelity:

* **Tables and columns**:
```sql
SELECT
    c.relname AS table_name,
    a.attname AS column_name,
    format_type(a.atttypid, a.atttypmod) AS full_type,
    t.typname AS base_type,
    NOT a.attnotnull AS is_nullable,
    pg_get_expr(d.adbin, d.adrelid) AS column_default,
    a.attnum AS ordinal_position
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t ON t.oid = a.atttypid
LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
WHERE n.nspname = $1
  AND c.relkind = 'r'
  AND a.attnum > 0
  AND NOT a.attisdropped
ORDER BY c.relname, a.attnum;
```

* **Indexes**:
```sql
SELECT
    t.relname AS table_name,
    i.relname AS index_name,
    ix.indisunique AS is_unique,
    pg_get_indexdef(ix.indexrelid) AS index_def
FROM pg_index ix
JOIN pg_class t ON t.oid = ix.indrelid
JOIN pg_class i ON i.oid = ix.indexrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
WHERE n.nspname = $1
  AND NOT ix.indisprimary
ORDER BY t.relname, i.relname;
```

* **Foreign keys**:
```sql
SELECT
    c.relname AS table_name,
    con.conname AS constraint_name,
    pg_get_constraintdef(con.oid) AS constraint_def
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1
  AND con.contype = 'f'
ORDER BY c.relname, con.conname;
```

### 2.2 SQLite catalog queries

On SQLite, metadata is introspected from `sqlite_schema` and system PRAGMAs:
* Tables: `SELECT name, sql FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'`
* Columns: `PRAGMA table_info(<table_name>)`
* Foreign keys: `PRAGMA foreign_key_list(<table_name>)`
* Indexes: `SELECT name, sql FROM sqlite_schema WHERE type='index' AND tbl_name = ?`

## 3. Type normalization and default sanitization

Dialects report internal type representations that differ from input DDL, causing false-positive diffs if compared directly.

Grizzle standardizes aliases to canonical types:

| Engine | Raw dialect name | Normalized form |
| :--- | :--- | :--- |
| PostgreSQL | `int4`, `serial` | `integer` |
| PostgreSQL | `int8`, `bigserial` | `bigint` |
| PostgreSQL | `int2`, `smallserial` | `smallint` |
| PostgreSQL | `bool` | `boolean` |
| PostgreSQL | `character varying(N)` | `varchar(N)` |
| PostgreSQL | `timestamp with time zone` | `timestamptz` |
| PostgreSQL | `timestamp without time zone` | `timestamp` |

### Default value normalization

PostgreSQL wraps defaults in outer parentheses and appends type cast suffixes:
* Raw catalog output: `('draft'::character varying)`
* Normalized output: `'draft'`
* Raw catalog output: `('2024-01-01'::date + '1 day'::interval)`
* Normalized output: `'2024-01-01'::date + '1 day'::interval`

To preserve arithmetic and compound expressions while removing unnecessary cast noise from single literals, Grizzle uses quote-aware parenthesis stripping and targets single-literal cast expressions (`'literal'::type`).

## 4. Diffing algorithm and scope filtering

The diff engine executes the following evaluation for each table:

1. **Table filtering**: Check if table matches `ExcludeTables`, built-in extension names (`spatial_ref_sys`), or is omitted from `IncludeTables`. If unmanaged, skip the table completely.
2. **Table existence**: If desired table does not exist in live schema, emit `ChangeCreateTable`.
3. **Column comparison**:
   * If desired column is absent in live table, emit `ChangeAddColumn`.
   * If column exists, check normalized type, nullability, and default value. If different, emit `ChangeAlterColumn`. Mark as destructive if narrowing types (e.g. `bigint` to `integer`).
   * For live columns missing from desired table, emit `ChangeDropColumn` marked as destructive.
4. **Index comparison**: Compare normalized index definitions. Emit `ChangeDropIndex` and `ChangeCreateIndex` as needed.
5. **Foreign key comparison**: Compare constraint definitions. Emit `ChangeDropFK` and `ChangeAddFK`.
6. **Dropped tables**: Emit `ChangeDropTable` for live managed tables missing from desired schema.

## 5. Topological statement ordering

Generated steps are ordered to prevent foreign key or dependency conflicts during execution:

```go
const (
    PriorityDropFK      = 10 // Drop foreign keys to remove cross-table references
    PriorityDropIndex   = 20 // Drop obsolete indexes
    PriorityCreateTable = 30 // Create new tables without foreign keys
    PriorityAddColumn   = 40 // Add new columns
    PriorityAlterColumn = 50 // Modify column types and constraints
    PriorityCreateIndex = 60 // Build new indexes
    PriorityAddFK       = 70 // Add foreign keys
    PriorityDropColumn  = 80 // Drop columns (if permitted by policy)
    PriorityDropTable   = 90 // Drop tables (if permitted by policy)
)
```

## 6. SQLite 12-step rebuild procedure

SQLite does not support altering column types or dropping columns natively. Grizzle executes the following migration sequence when changes require table reconstruction:

1. Set `PRAGMA foreign_keys = OFF;`
2. Create `_grizzle_rebuild_<table_name>` with the desired schema.
3. Copy intersecting columns:
   ```sql
   INSERT INTO "_grizzle_rebuild_users" ("id", "email")
   SELECT "id", "email" FROM "users";
   ```
4. Drop old table: `DROP TABLE "users";`
5. Rename new table: `ALTER TABLE "_grizzle_rebuild_users" RENAME TO "users";`
6. Re-create all indexes associated with the table.
7. Verify foreign key consistency with `PRAGMA foreign_key_check;`
8. Set `PRAGMA foreign_keys = ON;`
