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
	// pgxpool defaults MaxConns to max(4, NumCPU) — too small to absorb a
	// 67k-record backfill burst (~330 concurrent /ingest batches) while
	// also serving the dashboard's /summary + /users on the same pool;
	// /users would queue 10+ seconds behind the heavy /ingest CTEs.
	// Override only when the DSN didn't set pool_max_conns itself, so
	// operators retain explicit control via `?pool_max_conns=…`.
	if cfg.MaxConns < 25 {
		cfg.MaxConns = 25
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

// editInsertSQL mirrors insertSQL for file-edit events: unpack the JSON
// batch, INSERT into edit_detail (event_id dedup), roll only the
// actually-inserted rows into edit_daily. Same atomicity contract.
const editInsertSQL = `
WITH input AS (
    SELECT
        (e->>'event_id')::text                            AS event_id,
        COALESCE(e->>'session_id', '')::text              AS session_id,
        $2::text                                          AS user_id,
        $3::text                                          AS machine_id,
        COALESCE(NULLIF(e->>'tool', ''), 'claude-code')   AS tool,
        COALESCE(NULLIF(e->>'lang', ''), 'other')         AS lang,
        (e->>'timestamp')::timestamptz                    AS ts,
        COALESCE((e->>'lines_added')::bigint, 0)          AS lines_added,
        COALESCE((e->>'lines_removed')::bigint, 0)        AS lines_removed,
        NULLIF(e->>'project_path', '')                    AS project_path,
        COALESCE((e->>'backfill')::bool, false)           AS backfill,
        NOW()                                             AS received_at
    FROM jsonb_array_elements($1::jsonb) AS e
    WHERE COALESCE(e->>'event_id','') <> ''
),
ins AS (
    INSERT INTO edit_detail (
        event_id, session_id, user_id, machine_id, tool, lang, ts,
        lines_added, lines_removed, project_path, backfill, received_at)
    SELECT
        event_id, session_id, user_id, machine_id, tool, lang, ts,
        lines_added, lines_removed, project_path, backfill, received_at
    FROM input
    ON CONFLICT (event_id) DO NOTHING
    RETURNING (ts AT TIME ZONE 'UTC')::date AS day,
              user_id, machine_id, tool, lang, lines_added, lines_removed
),
agg AS (
    SELECT day, user_id, machine_id, tool, lang,
           SUM(lines_added)   AS added,
           SUM(lines_removed) AS removed,
           COUNT(*)           AS edits
    FROM ins
    GROUP BY 1, 2, 3, 4, 5
),
roll AS (
    INSERT INTO edit_daily (day, user_id, machine_id, tool, lang,
        lines_added, lines_removed, edits, updated_at)
    SELECT day, user_id, machine_id, tool, lang, added, removed, edits, NOW()
    FROM agg
    ON CONFLICT (day, user_id, machine_id, tool, lang) DO UPDATE SET
        lines_added   = edit_daily.lines_added   + EXCLUDED.lines_added,
        lines_removed = edit_daily.lines_removed + EXCLUDED.lines_removed,
        edits         = edit_daily.edits         + EXCLUDED.edits,
        updated_at    = NOW()
    RETURNING 1
)
SELECT (SELECT COUNT(*) FROM ins)::bigint   AS accepted,
       (SELECT COUNT(*) FROM input)::bigint AS considered
`

// InsertEdits writes one batch of file-edit events atomically; both
// edit_detail and edit_daily move forward together. Duplicate event_ids
// (re-scans, spool replays) are dropped by the detail PK and never
// double-count the daily rollup.
func (s *Store) InsertEdits(ctx context.Context, machineID, userID string, edits []types.EditRecord) (int, int, error) {
	if len(edits) == 0 {
		return 0, 0, nil
	}
	payload, err := json.Marshal(edits)
	if err != nil {
		return 0, 0, err
	}
	var accepted, considered int64
	err = s.pool.QueryRow(ctx, editInsertSQL, string(payload), userID, machineID).
		Scan(&accepted, &considered)
	if err != nil {
		return 0, 0, err
	}
	return int(accepted), int(considered - accepted), nil
}

// LangRow is one (day, user, tool, lang) aggregate of code-edit stats.
type LangRow struct {
	Day          string
	User         string
	Tool         string
	Lang         string
	LinesAdded   int64
	LinesRemoved int64
	Edits        int64
}

// LangAggregate returns per-day per-user per-tool per-language edit
// totals (UTC days) from the pre-aggregated edit_daily table. Filter
// semantics match Aggregate: empty user = all users, zero times = open
// range.
func (s *Store) LangAggregate(ctx context.Context, user string, from, to time.Time) ([]LangRow, error) {
	q := `
SELECT to_char(day, 'YYYY-MM-DD') AS day_s,
       user_id, tool, lang,
       SUM(lines_added), SUM(lines_removed), SUM(edits)
FROM edit_daily
WHERE ($1 = '' OR user_id = $1)
  AND ($2::date IS NULL OR day >= $2)
  AND ($3::date IS NULL OR day <  $3)
GROUP BY day, user_id, tool, lang
ORDER BY day DESC, user_id, tool, lang
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
	var out []LangRow
	for rows.Next() {
		var r LangRow
		if err := rows.Scan(&r.Day, &r.User, &r.Tool, &r.Lang,
			&r.LinesAdded, &r.LinesRemoved, &r.Edits); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
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

	rows, err := rebuildDailyTx(ctx, tx, from, to)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return rows, nil
}

func rebuildDailyTx(ctx context.Context, tx pgx.Tx, from, to time.Time) (int64, error) {
	from = time.Date(from.UTC().Year(), from.UTC().Month(), from.UTC().Day(), 0, 0, 0, 0, time.UTC)
	to = time.Date(to.UTC().Year(), to.UTC().Month(), to.UTC().Day(), 0, 0, 0, 0, time.UTC)
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

type ClockRow struct {
	DOW      int     // 0=Sunday .. 6=Saturday (Postgres EXTRACT(DOW))
	Hour     int     // 0..23 in the requested timezone
	Tokens   int64   // input + output + cache_creation + cache_read
	Messages int64   // one usage_detail row == one assistant message
	Cost     float64 // priced per detail row at its own ts, then summed
}

// ClockAggregate returns per-(weekday, hour) token/message totals with
// time-aware cost, bucketed in the given IANA timezone (e.g. "America/New_York";
// empty -> "UTC"). Unlike Aggregate this reads usage_detail directly because
// usage_daily has no hour grain — the ts range filter is index-supported
// (idx_detail_ts / idx_detail_user_ts) and the output is at most 7×24=168 rows.
// To stay fast it pre-aggregates detail into (local-date, hour, model) groups
// FIRST, then runs the time-aware model_prices lookup once per group (keyed on
// the local calendar date, same logic as Aggregate) — NOT once per detail row,
// which is what made the naive version slow. Models absent from model_prices
// contribute 0 cost (tokens still count); pricesync seeds defaults so that's rare.
func (s *Store) ClockAggregate(ctx context.Context, user, tz string, from, to time.Time) ([]ClockRow, error) {
	if tz == "" {
		tz = "UTC"
	}
	q := `
WITH agg AS (
    SELECT (d.ts AT TIME ZONE $4)::date                 AS d_local,
           EXTRACT(HOUR FROM d.ts AT TIME ZONE $4)::int AS hour,
           d.model,
           SUM(d.input_tokens)          AS in_t,
           SUM(d.output_tokens)         AS out_t,
           SUM(d.cache_creation_tokens) AS cc_t,
           SUM(d.cache_read_tokens)     AS cr_t,
           COUNT(*)                     AS msgs
    FROM usage_detail d
    WHERE ($1 = '' OR d.user_id = $1)
      AND ($2::timestamptz IS NULL OR d.ts >= $2)
      AND ($3::timestamptz IS NULL OR d.ts <  $3)
    GROUP BY 1, 2, 3
)
SELECT EXTRACT(DOW FROM a.d_local)::int AS dow,
       a.hour,
       SUM(a.in_t + a.out_t + a.cc_t + a.cr_t)::bigint AS tokens,
       SUM(a.msgs)::bigint                             AS messages,
       COALESCE(SUM(
           (a.in_t  * COALESCE(p.input_per_1m,          0)
          + a.out_t * COALESCE(p.output_per_1m,         0)
          + a.cc_t  * COALESCE(p.cache_creation_per_1m, 0)
          + a.cr_t  * COALESCE(p.cache_read_per_1m,     0)) / 1000000.0), 0) AS cost
FROM agg a
LEFT JOIN LATERAL (
    SELECT mp.input_per_1m, mp.output_per_1m,
           mp.cache_creation_per_1m, mp.cache_read_per_1m
    FROM model_prices mp
    WHERE a.model LIKE mp.model_prefix || '%'
      AND mp.valid_from <= (a.d_local::timestamptz + interval '1 day')
      AND (mp.valid_to IS NULL OR mp.valid_to > a.d_local::timestamptz)
    ORDER BY length(mp.model_prefix) DESC, mp.valid_from DESC
    LIMIT 1
) p ON TRUE
GROUP BY 1, 2
ORDER BY 1, 2
`
	var fromArg, toArg any
	if !from.IsZero() {
		fromArg = from.UTC()
	}
	if !to.IsZero() {
		toArg = to.UTC()
	}
	rows, err := s.pool.Query(ctx, q, user, fromArg, toArg, tz)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClockRow
	for rows.Next() {
		var r ClockRow
		if err := rows.Scan(&r.DOW, &r.Hour, &r.Tokens, &r.Messages, &r.Cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
