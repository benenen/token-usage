package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"tokenusage/internal/types"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// schema covers four tables:
//   - users        : identity. Watchers don't carry user_id; they auth with
//                    an API key and the server resolves to the user.
//   - api_keys     : each user may mint multiple keys (per machine, per
//                    rotation). We store sha256(key), never the plaintext.
//   - usage_detail : one row per assistant message (the source of truth).
//   - usage_daily  : pre-aggregated per (day, user, machine, tool, model).
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
    key_hash     TEXT PRIMARY KEY,                       -- sha256(raw) hex
    key_prefix   TEXT NOT NULL,                          -- "tuk_" + first 8 chars, for display
    user_id      TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    name         TEXT,                                   -- optional human label
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_api_keys_user   ON api_keys(user_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_prefix ON api_keys(key_prefix);

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
`

// insertSQL writes one batch in a single round trip:
//  1. Unpack the JSON payload into a typed rowset (`input`).
//  2. INSERT into usage_detail with ON CONFLICT DO NOTHING; only rows
//     that were actually accepted leave the `ins` CTE.
//  3. GROUP those rows by (day, user, machine, tool, model).
//  4. UPSERT into usage_daily, ADDing the per-batch deltas to existing
//     counters. LEAST/GREATEST keep first_ts/last_ts coherent.
//
// Returns (accepted_count, considered_count); duplicates = considered - accepted.
const insertSQL = `
WITH input AS (
    SELECT
        (e->>'message_id')::text                                  AS message_id,
        COALESCE(e->>'request_id', '')::text                      AS request_id,
        (e->>'session_id')::text                                  AS session_id,
        $2::text                                                  AS user_id,
        $3::text                                                  AS machine_id,
        COALESCE(NULLIF(e->>'tool', ''), 'claude-code')           AS tool,
        (e->>'model')::text                                       AS model,
        (e->>'timestamp')::timestamptz                            AS ts,
        COALESCE((e->>'input_tokens')::bigint, 0)                 AS input_tokens,
        COALESCE((e->>'output_tokens')::bigint, 0)                AS output_tokens,
        COALESCE((e->>'cache_creation_tokens')::bigint, 0)        AS cache_creation_tokens,
        COALESCE((e->>'cache_read_tokens')::bigint, 0)            AS cache_read_tokens,
        NULLIF(e->>'project_path', '')                            AS project_path,
        COALESCE((e->>'backfill')::bool, false)                   AS backfill,
        NOW()                                                     AS received_at
    FROM jsonb_array_elements($1::jsonb) AS e
    WHERE COALESCE(e->>'message_id','') <> ''
),
ins AS (
    INSERT INTO usage_detail (
        message_id, request_id, session_id, user_id, machine_id, tool, model, ts,
        input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
        project_path, backfill, received_at)
    SELECT
        message_id, request_id, session_id, user_id, machine_id, tool, model, ts,
        input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
        project_path, backfill, received_at
    FROM input
    ON CONFLICT (message_id, request_id) DO NOTHING
    RETURNING (ts AT TIME ZONE 'UTC')::date AS day,
              user_id, machine_id, tool, model,
              input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, ts
),
agg AS (
    SELECT day, user_id, machine_id, tool, model,
           SUM(input_tokens)          AS in_t,
           SUM(output_tokens)         AS out_t,
           SUM(cache_creation_tokens) AS cc_t,
           SUM(cache_read_tokens)     AS cr_t,
           COUNT(*)                   AS msgs,
           MIN(ts)                    AS mn,
           MAX(ts)                    AS mx
    FROM ins
    GROUP BY 1, 2, 3, 4, 5
),
roll AS (
    INSERT INTO usage_daily (day, user_id, machine_id, tool, model,
        input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
        messages, first_ts, last_ts, updated_at)
    SELECT day, user_id, machine_id, tool, model, in_t, out_t, cc_t, cr_t, msgs, mn, mx, NOW()
    FROM agg
    ON CONFLICT (day, user_id, machine_id, tool, model) DO UPDATE SET
        input_tokens          = usage_daily.input_tokens          + EXCLUDED.input_tokens,
        output_tokens         = usage_daily.output_tokens         + EXCLUDED.output_tokens,
        cache_creation_tokens = usage_daily.cache_creation_tokens + EXCLUDED.cache_creation_tokens,
        cache_read_tokens     = usage_daily.cache_read_tokens     + EXCLUDED.cache_read_tokens,
        messages              = usage_daily.messages              + EXCLUDED.messages,
        first_ts              = LEAST(usage_daily.first_ts,    EXCLUDED.first_ts),
        last_ts               = GREATEST(usage_daily.last_ts, EXCLUDED.last_ts),
        updated_at            = NOW()
    RETURNING 1
)
SELECT (SELECT COUNT(*) FROM ins)::bigint   AS accepted,
       (SELECT COUNT(*) FROM input)::bigint AS considered,
       (SELECT COUNT(*) FROM roll)::bigint  AS rolled
`

// Insert writes a batch atomically: both usage_detail and usage_daily
// move forward together (or both roll back on error).
//
// Each record carries its own `tool` field; rows missing it are stamped
// 'claude-code' by the SQL coalesce. user_id and machine_id come from
// the envelope (they describe the watcher, not the work).
func (s *Store) Insert(ctx context.Context, machineID, userID string, recs []types.UsageRecord) (int, int, error) {
	if len(recs) == 0 {
		return 0, 0, nil
	}
	payload, err := json.Marshal(recs)
	if err != nil {
		return 0, 0, err
	}
	var accepted, considered, rolled int64
	err = s.pool.QueryRow(ctx, insertSQL, string(payload), userID, machineID).
		Scan(&accepted, &considered, &rolled)
	if err != nil {
		return 0, 0, err
	}
	_ = rolled // currently unused; useful for future telemetry
	return int(accepted), int(considered - accepted), nil
}

type AggRow struct {
	Day      string
	User     string
	Model    string
	Input    int64
	Output   int64
	CacheCC  int64
	CacheRR  int64
	Messages int64
}

// Aggregate returns per-day per-user per-model token totals (UTC days),
// served straight off the pre-aggregated usage_daily table.
func (s *Store) Aggregate(ctx context.Context, user string, from, to time.Time) ([]AggRow, error) {
	q := `SELECT to_char(day, 'YYYY-MM-DD') AS day_s,
                 user_id, model,
                 SUM(input_tokens), SUM(output_tokens),
                 SUM(cache_creation_tokens), SUM(cache_read_tokens),
                 SUM(messages)
          FROM usage_daily
          WHERE ($1 = '' OR user_id = $1)
            AND ($2::date IS NULL OR day >= $2)
            AND ($3::date IS NULL OR day <  $3)
          GROUP BY day, user_id, model
          ORDER BY day DESC, user_id, model`
	var fromArg, toArg any
	if !from.IsZero() {
		fromArg = from.UTC()
	}
	if !to.IsZero() {
		toArg = to.UTC()
	}
	rows, err := s.pool.Query(ctx, q, user, fromArg, toArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AggRow
	for rows.Next() {
		var r AggRow
		if err := rows.Scan(&r.Day, &r.User, &r.Model, &r.Input, &r.Output, &r.CacheCC, &r.CacheRR, &r.Messages); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
