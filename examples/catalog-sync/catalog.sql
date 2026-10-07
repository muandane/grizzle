-- catalog.sql — desired cluster-catalog state for the catalog-sync example.
--
-- Usage (PostgreSQL):
--
--   grizzle apply --dsn $DATABASE_URL --schema examples/catalog-sync/schema.sql \
--     --catalog examples/catalog-sync/catalog.sql
--
-- Contract (docs/SPEC.md §2.2 "CatalogSQL contract"):
--   * The same catalog statements may be placed in SchemaSQL. This optional
--     --catalog file is authoritative when a publication/event-trigger name
--     is duplicated. CREATE/ALTER/DROP PUBLICATION and CREATE/ALTER/DROP
--     EVENT TRIGGER statements are accepted; anything else fails with
--     ErrInvalidOptions instead of being silently ignored. This file is
--     never shadow-compiled.
--   * Publications are diffed on table membership (FOR TABLE / FOR ALL
--     TABLES / FOR TABLES IN SCHEMA — the latter requires PostgreSQL 15+)
--     and publish flags. A missing WITH (publish = ...) clause means all
--     four DML operations.
--   * Event triggers are diffed on event, WHEN TAG IN filter, function, and
--     enabled state. The trigger function must exist (manage it in
--     SchemaSQL); creating a trigger whose function is missing aborts the
--     sync. Definition drift is DROP+CREATE; toggling Enabled is applied as
--     ALTER EVENT TRIGGER ENABLE/DISABLE.
--   * Objects Grizzle creates are stamped with a "grizzle-managed" catalog
--     comment. Only marker-stamped objects that leave this file are
--     dropped, and only behind AllowDropPublication / AllowDropEventTrigger
--     plus the DROP_PUBLICATION / DROP_EVENT_TRIGGER critical hazards.
--     Operator-created publications and event triggers are never swept.
--   * CREATE EVENT TRIGGER usually requires superuser; every event-trigger
--     step carries an EVENT_TRIGGER_SUPERUSER warning, and FOR ALL TABLES
--     carries a PUBLICATION_ALL_TABLES notice.

-- Publish the docs table (unqualified names resolve against the target
-- schema) with default publish flags.
CREATE PUBLICATION docs_pub FOR TABLE docs;

-- Publish an entire schema (PostgreSQL 15+):
-- CREATE PUBLICATION analytics_pub FOR TABLES IN SCHEMA analytics;

-- Publish every table in the database, including future ones (NOTICE):
-- CREATE PUBLICATION all_pub FOR ALL TABLES;

-- Narrow publish flags: only INSERT leaves the database.
-- CREATE PUBLICATION ins_only WITH (publish = 'insert');

-- DDL audit trigger: the function must be managed in SchemaSQL.
CREATE EVENT TRIGGER audit_ddl ON ddl_command_end EXECUTE FUNCTION log_ddl();

-- Tag-filtered trigger that refuses dropped tables (needs a rule-breaking
-- function such as one that raises an exception):
-- CREATE EVENT TRIGGER block_table_drop ON sql_drop
--   WHEN TAG IN ('DROP TABLE')
--   EXECUTE FUNCTION refuse_ddl();
