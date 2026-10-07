CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE docs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title text NOT NULL,
    owner_name text NOT NULL DEFAULT 'system',
    fingerprint text GENERATED ALWAYS AS (encode(digest(title, 'sha256'), 'hex')) STORED,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION touch_ts() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    NEW.created_at := now();
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER docs_touch BEFORE UPDATE ON docs
FOR EACH ROW EXECUTE FUNCTION touch_ts();

CREATE FUNCTION doc_count() RETURNS bigint LANGUAGE sql STABLE AS
$fn$ SELECT count(*) FROM docs; $fn$;

ALTER TABLE docs ENABLE ROW LEVEL SECURITY;

CREATE POLICY docs_owner_select ON docs FOR SELECT USING (owner_name = current_user);

CREATE VIEW docs_titles AS SELECT id, title FROM docs;

CREATE MATERIALIZED VIEW docs_summary AS
SELECT count(*) AS total, max(id) AS max_id FROM docs;
