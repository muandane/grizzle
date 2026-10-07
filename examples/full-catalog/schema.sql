CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE DOMAIN positive_int AS integer DEFAULT 0 CHECK (VALUE > 0);

CREATE TABLE docs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title text NOT NULL,
    owner_name text NOT NULL DEFAULT 'system',
    weight positive_int NOT NULL DEFAULT 1,
    fingerprint text GENERATED ALWAYS AS (encode(digest(title, 'sha256'), 'hex')) STORED,
    created_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE docs IS 'Managed documents; fingerprint is a derived integrity hash';
COMMENT ON COLUMN docs.owner_name IS 'Role responsible for the document';

CREATE FUNCTION touch_ts() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    NEW.created_at := now();
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER docs_touch BEFORE UPDATE ON docs
FOR EACH ROW EXECUTE FUNCTION touch_ts();

CREATE PROCEDURE rotate_owner(doc_id bigint, new_owner text) LANGUAGE sql AS $p$
UPDATE docs SET owner_name = new_owner WHERE id = doc_id;
$p$;

CREATE FUNCTION doc_count() RETURNS bigint LANGUAGE sql STABLE AS
$fn$ SELECT count(*) FROM docs; $fn$;

ALTER TABLE docs ENABLE ROW LEVEL SECURITY;

CREATE POLICY docs_owner_select ON docs FOR SELECT USING (owner_name = current_user);

CREATE VIEW docs_titles AS SELECT id, title FROM docs;

CREATE MATERIALIZED VIEW docs_summary AS
SELECT count(*) AS total, max(id) AS max_id FROM docs;
