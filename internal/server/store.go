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

CREATE TABLE IF NOT EXISTS model_prices (
    model_prefix          TEXT             NOT NULL,
    valid_from            TIMESTAMPTZ      NOT NULL,
    valid_to              TIMESTAMPTZ,             -- NULL = currently in effect
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

// ----- model_prices CRUD ----------------------------------------------------

type PriceRow struct {
	ModelPrefix   string
	ValidFrom     time.Time
	ValidTo       *time.Time
	InputPer1M    float64
	OutputPer1M   float64
	CacheCreate1M float64
	CacheRead1M   float64
	Source        string
	FetchedAt     time.Time
}

// epoch is the synthetic valid_from used for "this price has been in
// effect since forever". When LiteLLM sees a model for the first time
// we backfill to 1970 so existing usage data is priced at real (not
// stale-default) rates from the get-go — that's the difference between
// our /summary and a fresh `ccusage` run.
var epoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// UpsertPrice writes a new price row when rates differ from the current
// active row for the same model_prefix. Three cases:
//
//  1. No active row for the prefix yet → INSERT with valid_from=epoch.
//     The new price covers all historical usage (matches ccusage's
//     "current LiteLLM price for all data" behavior on first sync).
//
//  2. Active row exists and is a default-seed placeholder → UPDATE in
//     place (replace the seed, keep its valid_from=epoch). Default
//     seeds are stand-ins for "we don't know yet"; once we have real
//     data, the placeholder should vanish silently.
//
//  3. Active row is a real LiteLLM/manual entry whose rates changed →
//     close it (valid_to=NOW) and INSERT a new row (valid_from=NOW).
//     This is the only path that produces a price-change history step.
//
// Identical rates → no-op (idempotent).
func (s *Store) UpsertPrice(ctx context.Context, p PriceRow) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var (
		curIn, curOut, curCC, curCR float64
		curSource                   string
		found                       bool
	)
	err = tx.QueryRow(ctx, `
		SELECT input_per_1m, output_per_1m, cache_creation_per_1m, cache_read_per_1m, source
		FROM model_prices
		WHERE model_prefix = $1 AND valid_to IS NULL
	`, p.ModelPrefix).Scan(&curIn, &curOut, &curCC, &curCR, &curSource)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		found = false
	case err != nil:
		return false, err
	default:
		found = true
	}
	if found && curIn == p.InputPer1M && curOut == p.OutputPer1M &&
		curCC == p.CacheCreate1M && curCR == p.CacheRead1M {
		return false, nil
	}
	src := p.Source
	if src == "" {
		src = "manual"
	}

	// Case 2: replace a default-seed in place.
	if found && curSource == "default-seed" {
		if _, err := tx.Exec(ctx, `
			UPDATE model_prices SET
			    input_per_1m = $2, output_per_1m = $3,
			    cache_creation_per_1m = $4, cache_read_per_1m = $5,
			    source = $6, fetched_at = NOW()
			WHERE model_prefix = $1 AND valid_to IS NULL
		`, p.ModelPrefix, p.InputPer1M, p.OutputPer1M,
			p.CacheCreate1M, p.CacheRead1M, src); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}

	// Case 1: first authoritative price for this prefix.
	from := p.ValidFrom
	if !found {
		from = epoch
	} else if from.IsZero() {
		from = time.Now() // Case 3 default
	}

	// Case 3: close the old version, then insert the new one below.
	if found {
		if _, err := tx.Exec(ctx, `
			UPDATE model_prices SET valid_to = NOW()
			WHERE model_prefix = $1 AND valid_to IS NULL
		`, p.ModelPrefix); err != nil {
			return false, err
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO model_prices
		    (model_prefix, valid_from, input_per_1m, output_per_1m,
		     cache_creation_per_1m, cache_read_per_1m, source, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (model_prefix, valid_from) DO UPDATE SET
		    input_per_1m = EXCLUDED.input_per_1m,
		    output_per_1m = EXCLUDED.output_per_1m,
		    cache_creation_per_1m = EXCLUDED.cache_creation_per_1m,
		    cache_read_per_1m = EXCLUDED.cache_read_per_1m,
		    source = EXCLUDED.source,
		    fetched_at = NOW()
	`, p.ModelPrefix, from, p.InputPer1M, p.OutputPer1M,
		p.CacheCreate1M, p.CacheRead1M, src); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ListActivePrices returns the currently-in-effect price for each model.
func (s *Store) ListActivePrices(ctx context.Context) ([]PriceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT model_prefix, valid_from, valid_to,
		       input_per_1m, output_per_1m,
		       cache_creation_per_1m, cache_read_per_1m,
		       source, fetched_at
		FROM model_prices
		WHERE valid_to IS NULL
		ORDER BY model_prefix
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPriceRows(rows)
}

// ListPriceHistory returns every price row (active + historical) for the
// given prefix, ordered oldest-first. Empty prefix returns all models.
func (s *Store) ListPriceHistory(ctx context.Context, modelPrefix string) ([]PriceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT model_prefix, valid_from, valid_to,
		       input_per_1m, output_per_1m,
		       cache_creation_per_1m, cache_read_per_1m,
		       source, fetched_at
		FROM model_prices
		WHERE ($1 = '' OR model_prefix = $1)
		ORDER BY model_prefix, valid_from
	`, modelPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPriceRows(rows)
}

func scanPriceRows(rows pgx.Rows) ([]PriceRow, error) {
	var out []PriceRow
	for rows.Next() {
		var r PriceRow
		if err := rows.Scan(&r.ModelPrefix, &r.ValidFrom, &r.ValidTo,
			&r.InputPer1M, &r.OutputPer1M,
			&r.CacheCreate1M, &r.CacheRead1M,
			&r.Source, &r.FetchedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeedDefaultPrices inserts the in-memory default rates with a very-old
// valid_from if and only if model_prices is empty. This guarantees
// /summary's LATERAL JOIN always finds a price even before the first
// LiteLLM sync runs.
func (s *Store) SeedDefaultPrices(ctx context.Context, defaults map[string]Rate) error {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model_prices`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for prefix, r := range defaults {
		if _, err := tx.Exec(ctx, `
			INSERT INTO model_prices
			    (model_prefix, valid_from, input_per_1m, output_per_1m,
			     cache_creation_per_1m, cache_read_per_1m, source, fetched_at)
			VALUES ($1, '1970-01-01 00:00:00+00', $2, $3, $4, $5, 'default-seed', NOW())
			ON CONFLICT DO NOTHING
		`, prefix, r.Input, r.Output, r.CacheCreation, r.CacheRead); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RebuildDaily replaces every usage_daily row whose `day` falls in [from, to]
// (UTC, both inclusive) with a fresh aggregate computed from usage_detail.
// Returns the number of daily rows written.
//
// Concurrency: runs in a single tx. The DELETE acquires row-level locks
// on the affected daily rows; concurrent /ingest UPSERTs to the same
// (day, user, machine, tool, model) keys block until commit. After we
// commit, the blocked UPSERT proceeds with += on top of our fresh row,
// adding only its own delta — no double counting.
func (s *Store) RebuildDaily(ctx context.Context, from, to time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM usage_daily WHERE day >= $1 AND day <= $2`,
		from, to,
	); err != nil {
		return 0, err
	}
	res, err := tx.Exec(ctx, `
		INSERT INTO usage_daily (day, user_id, machine_id, tool, model,
		    input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		    messages, first_ts, last_ts, updated_at)
		SELECT (ts AT TIME ZONE 'UTC')::date,
		       user_id, machine_id, tool, model,
		       SUM(input_tokens), SUM(output_tokens),
		       SUM(cache_creation_tokens), SUM(cache_read_tokens),
		       COUNT(*), MIN(ts), MAX(ts), NOW()
		FROM usage_detail
		WHERE (ts AT TIME ZONE 'UTC')::date >= $1
		  AND (ts AT TIME ZONE 'UTC')::date <= $2
		GROUP BY 1, 2, 3, 4, 5
	`, from, to)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

type AggRow struct {
	Day      string
	User     string
	Tool     string
	Model    string
	Input    int64
	Output   int64
	CacheCC  int64
	CacheRR  int64
	Messages int64
	Cost     float64 // time-aware cost computed in SQL using model_prices valid on Day
}

// Aggregate returns per-day per-user per-tool per-model token totals (UTC
// days) with time-aware cost. For each daily row, a LATERAL subquery
// looks up the model_prices entry that was valid on that day (longest
// matching prefix, latest valid_from) — so changing today's prices does
// not retroactively re-price historical rows. If no price is registered,
// cost is 0 (and the caller may fall back to the in-memory Pricer).
func (s *Store) Aggregate(ctx context.Context, user string, from, to time.Time) ([]AggRow, error) {
	q := `
SELECT to_char(d.day, 'YYYY-MM-DD')                       AS day_s,
       d.user_id, d.tool, d.model,
       SUM(d.input_tokens),
       SUM(d.output_tokens),
       SUM(d.cache_creation_tokens),
       SUM(d.cache_read_tokens),
       SUM(d.messages),
       COALESCE(
         SUM(d.input_tokens          * p.input_per_1m         ) / 1000000.0, 0) +
       COALESCE(
         SUM(d.output_tokens         * p.output_per_1m        ) / 1000000.0, 0) +
       COALESCE(
         SUM(d.cache_creation_tokens * p.cache_creation_per_1m) / 1000000.0, 0) +
       COALESCE(
         SUM(d.cache_read_tokens     * p.cache_read_per_1m    ) / 1000000.0, 0) AS cost_usd
FROM usage_daily d
LEFT JOIN LATERAL (
    SELECT mp.input_per_1m, mp.output_per_1m,
           mp.cache_creation_per_1m, mp.cache_read_per_1m
    FROM model_prices mp
    WHERE d.model LIKE mp.model_prefix || '%'
      AND mp.valid_from <= (d.day::timestamptz + interval '1 day')
      AND (mp.valid_to IS NULL OR mp.valid_to > d.day::timestamptz)
    ORDER BY length(mp.model_prefix) DESC, mp.valid_from DESC
    LIMIT 1
) p ON TRUE
WHERE ($1 = '' OR d.user_id = $1)
  AND ($2::date IS NULL OR d.day >= $2)
  AND ($3::date IS NULL OR d.day <  $3)
GROUP BY d.day, d.user_id, d.tool, d.model
ORDER BY d.day DESC, d.user_id, d.tool, d.model
`
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
		if err := rows.Scan(&r.Day, &r.User, &r.Tool, &r.Model,
			&r.Input, &r.Output, &r.CacheCC, &r.CacheRR, &r.Messages, &r.Cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
