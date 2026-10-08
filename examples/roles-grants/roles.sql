-- roles.sql — desired role/privilege state for the roles-grants example.
--
-- Usage (PostgreSQL):
--
--   grizzle apply --dsn $DATABASE_URL --schema examples/roles-grants/schema.sql \
--     --roles examples/roles-grants/roles.sql
--
-- Contract (docs/SPEC.md §2.2 "RolesSQL contract"):
--   * The same role statements may be placed in SchemaSQL. This optional
--     --roles file is authoritative when a role/grant identity is duplicated.
--   * CREATE/ALTER ROLE/USER (LOGIN/PASSWORD/config attrs) and object-privilege
--     GRANT/REVOKE statements are accepted. DROP ROLE/USER and
--     SUPERUSER/REPLICATION/BYPASSRLS fail with ErrInvalidOptions.
--   * Omitting LOGIN/NOLOGIN keeps PostgreSQL's CREATE ROLE default (NOLOGIN).
--     Passwords are managed via hash compare and never written into Step.SQL.
--   * Roles Grizzle creates are stamped with a "grizzle-managed" catalog
--     comment. Only marker-stamped roles that leave this file are dropped,
--     and only behind AllowDropRole + the DROP_ROLE critical hazard.
--   * Surplus grants are revoked only behind AllowRevoke + REVOKE_PRIVILEGE.
--   * Grants to PUBLIC or to roles Grizzle does not manage are never revoked.
--   * ALL expands to the kind-specific full privilege set (TABLE: 7 privileges,
--     SEQUENCE: USAGE/SELECT/UPDATE, DATABASE: CONNECT/CREATE/TEMPORARY,
--     SCHEMA: USAGE/CREATE, FUNCTION: EXECUTE).

CREATE ROLE app_read;
CREATE ROLE app_writer;

-- Read-only access to the docs table.
GRANT SELECT ON docs TO app_read;

-- Full row CRUD on docs, plus the sequence backing its primary key.
GRANT SELECT, INSERT, UPDATE, DELETE ON docs TO app_writer;
GRANT USAGE ON SEQUENCE docs_id_seq TO app_writer;

-- Schema and database level access.
GRANT USAGE ON SCHEMA public TO app_read, app_writer;
GRANT CONNECT ON DATABASE app TO app_read, app_writer;

-- Function execution.
GRANT EXECUTE ON FUNCTION notify_event() TO app_writer;

-- A grant WITH GRANT OPTION lets the grantee re-grant the privilege;
-- revoking it renders REVOKE GRANT OPTION FOR ...
-- GRANT SELECT ON docs TO auditor WITH GRANT OPTION;
