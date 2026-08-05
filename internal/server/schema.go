package server

// schema covers four tables:
//   - users: identity. Watchers authenticate with an API key.
//   - api_keys: hashed watcher credentials.
//   - usage_detail / usage_daily: token source of truth and daily rollup.
//   - edit_detail / edit_daily: code-edit source of truth and daily rollup.
//
// usage_detail.user_id is plain text (no FK to users) so historical rows
// from pre-auth deployments stay queryable.
const schema = `
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_class WHERE relname='usage' AND relkind='r')
     AND NOT EXISTS (SELECT 1 FROM pg_class WHERE relname='usage_detail' AND relkind='r') THEN
    EXECUTE 'ALTER TABLE usage RENAME TO usage_detail';
    EXECUTE 'DROP INDEX IF EXISTS idx_usage_user_ts';
    EXECUTE 'DROP INDEX IF EXISTS idx_usage_session';
    EXECUTE 'DROP INDEX IF EXISTS idx_usage_ts';
    EXECUTE 'DROP INDEX IF EXISTS idx_usage_machine';
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS users (
    user_id     TEXT PRIMARY KEY,
    email       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    disabled_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS api_keys (
    key_hash     TEXT PRIMARY KEY,
    key_prefix   TEXT NOT NULL,
    user_id      TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    name         TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_api_keys_user   ON api_keys(user_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_prefix ON api_keys(key_prefix);

CREATE TABLE IF NOT EXISTS model_prices (
    model_prefix          TEXT             NOT NULL,
    valid_from            TIMESTAMPTZ      NOT NULL,
    valid_to              TIMESTAMPTZ,
    input_per_1m          DOUBLE PRECISION NOT NULL,
    output_per_1m         DOUBLE PRECISION NOT NULL,
    cache_creation_per_1m DOUBLE PRECISION NOT NULL DEFAULT 0,
    cache_read_per_1m     DOUBLE PRECISION NOT NULL DEFAULT 0,
    source                TEXT             NOT NULL DEFAULT 'manual',
    fetched_at            TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    PRIMARY KEY (model_prefix, valid_from)
);
CREATE INDEX IF NOT EXISTS idx_model_prices_active ON model_prices(model_prefix) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS idx_model_prices_range  ON model_prices(model_prefix, valid_from);

CREATE TABLE IF NOT EXISTS usage_detail (
    message_id            TEXT        NOT NULL,
    request_id            TEXT        NOT NULL DEFAULT '',
    session_id            TEXT        NOT NULL,
    user_id               TEXT        NOT NULL,
    machine_id            TEXT        NOT NULL,
    tool                  TEXT        NOT NULL DEFAULT 'claude-code',
    model                 TEXT        NOT NULL,
    ts                    TIMESTAMPTZ NOT NULL,
    input_tokens          BIGINT      NOT NULL,
    output_tokens         BIGINT      NOT NULL,
    cache_creation_tokens BIGINT      NOT NULL,
    cache_read_tokens     BIGINT      NOT NULL,
    project_path          TEXT,
    backfill              BOOLEAN     NOT NULL DEFAULT FALSE,
    received_at           TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (message_id, request_id)
);
ALTER TABLE usage_detail ADD COLUMN IF NOT EXISTS tool TEXT NOT NULL DEFAULT 'claude-code';

CREATE INDEX IF NOT EXISTS idx_detail_user_ts ON usage_detail(user_id, ts);
CREATE INDEX IF NOT EXISTS idx_detail_session ON usage_detail(session_id);
CREATE INDEX IF NOT EXISTS idx_detail_ts      ON usage_detail(ts);
CREATE INDEX IF NOT EXISTS idx_detail_machine ON usage_detail(machine_id, ts);
CREATE INDEX IF NOT EXISTS idx_detail_tool    ON usage_detail(tool, ts);

CREATE TABLE IF NOT EXISTS usage_daily (
    day                   DATE        NOT NULL,
    user_id               TEXT        NOT NULL,
    machine_id            TEXT        NOT NULL,
    tool                  TEXT        NOT NULL,
    model                 TEXT        NOT NULL,
    input_tokens          BIGINT      NOT NULL DEFAULT 0,
    output_tokens         BIGINT      NOT NULL DEFAULT 0,
    cache_creation_tokens BIGINT      NOT NULL DEFAULT 0,
    cache_read_tokens     BIGINT      NOT NULL DEFAULT 0,
    messages              BIGINT      NOT NULL DEFAULT 0,
    first_ts              TIMESTAMPTZ,
    last_ts               TIMESTAMPTZ,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (day, user_id, machine_id, tool, model)
);
CREATE INDEX IF NOT EXISTS idx_daily_day  ON usage_daily(day);
CREATE INDEX IF NOT EXISTS idx_daily_user ON usage_daily(user_id, day);
CREATE INDEX IF NOT EXISTS idx_daily_tool ON usage_daily(tool, day);

-- One-time bootstrap if daily is empty but detail has rows
INSERT INTO usage_daily (day, user_id, machine_id, tool, model,
    input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
    messages, first_ts, last_ts)
SELECT (ts AT TIME ZONE 'UTC')::date,
       user_id, machine_id, tool, model,
       SUM(input_tokens), SUM(output_tokens),
       SUM(cache_creation_tokens), SUM(cache_read_tokens),
       COUNT(*), MIN(ts), MAX(ts)
FROM usage_detail
WHERE NOT EXISTS (SELECT 1 FROM usage_daily LIMIT 1)
GROUP BY 1, 2, 3, 4, 5
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS edit_detail (
    event_id      TEXT        PRIMARY KEY,
    session_id    TEXT        NOT NULL,
    user_id       TEXT        NOT NULL,
    machine_id    TEXT        NOT NULL,
    tool          TEXT        NOT NULL DEFAULT 'claude-code',
    lang          TEXT        NOT NULL DEFAULT 'other',
    ts            TIMESTAMPTZ NOT NULL,
    lines_added   BIGINT      NOT NULL DEFAULT 0,
    lines_removed BIGINT      NOT NULL DEFAULT 0,
    project_path  TEXT,
    backfill      BOOLEAN     NOT NULL DEFAULT FALSE,
    received_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_edit_detail_user_ts ON edit_detail(user_id, ts);
CREATE INDEX IF NOT EXISTS idx_edit_detail_ts      ON edit_detail(ts);

CREATE TABLE IF NOT EXISTS edit_daily (
    day           DATE        NOT NULL,
    user_id       TEXT        NOT NULL,
    machine_id    TEXT        NOT NULL,
    tool          TEXT        NOT NULL,
    lang          TEXT        NOT NULL,
    lines_added   BIGINT      NOT NULL DEFAULT 0,
    lines_removed BIGINT      NOT NULL DEFAULT 0,
    edits         BIGINT      NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (day, user_id, machine_id, tool, lang)
);
CREATE INDEX IF NOT EXISTS idx_edit_daily_day  ON edit_daily(day);
CREATE INDEX IF NOT EXISTS idx_edit_daily_user ON edit_daily(user_id, day);
`
