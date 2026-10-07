-- Declarative schema: Single source of truth (desired end state)
--
-- This release renames users.full_name to users.display_name.
-- Grizzle plans this in two zero-downtime stages (see main.go):
--   1. expand:   add display_name alongside full_name (nullable), backfill
--   2. contract: drop full_name in a separately approved plan
CREATE TABLE users (
    id BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL UNIQUE,
    display_name TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_users_display_name ON users (display_name);