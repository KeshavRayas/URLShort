-- Schema is applied at startup and is idempotent. For a larger system this
-- would be a versioned migration tool; here the DDL is small and additive.

CREATE TABLE IF NOT EXISTS users (
    id           uuid PRIMARY KEY,
    name         text NOT NULL,
    api_key_hash text NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS links (
    code       text PRIMARY KEY CHECK (char_length(code) BETWEEN 3 AND 32),
    target_url text NOT NULL,
    user_id    uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    deleted_at timestamptz,
    clicks     bigint NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS links_user_created_idx
    ON links (user_id, created_at DESC);

-- Partial index keeps the resolve path off soft-deleted rows.
CREATE INDEX IF NOT EXISTS links_live_code_idx
    ON links (code)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS click_events (
    id           bigserial PRIMARY KEY,
    code         text NOT NULL,
    clicked_at   timestamptz NOT NULL DEFAULT now(),
    referer      text NOT NULL DEFAULT '',
    user_agent   text NOT NULL DEFAULT '',
    visitor_hash text NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS click_events_code_time_idx
    ON click_events (code, clicked_at DESC);
