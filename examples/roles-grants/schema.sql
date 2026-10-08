-- schema.sql — desired schema state for the roles-grants example.
--
-- Objects referenced by examples/roles-grants/roles.sql must exist here
-- (or already be live) before privilege sync runs.

CREATE TABLE docs (
    id BIGSERIAL PRIMARY KEY,
    body text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Target of GRANT EXECUTE in roles.sql.
CREATE FUNCTION notify_event() RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('docs_events', 'ping');
END;
$$;
