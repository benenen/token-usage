package server

import (
	"context"
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

const schema = `
CREATE TABLE IF NOT EXISTS usage (
    message_id            TEXT        NOT NULL,
    request_id            TEXT        NOT NULL DEFAULT '',
    session_id            TEXT        NOT NULL,
    user_id               TEXT        NOT NULL,
    machine_id            TEXT        NOT NULL,
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
CREATE INDEX IF NOT EXISTS idx_usage_user_ts  ON usage(user_id, ts);
CREATE INDEX IF NOT EXISTS idx_usage_session  ON usage(session_id);
CREATE INDEX IF NOT EXISTS idx_usage_ts       ON usage(ts);
CREATE INDEX IF NOT EXISTS idx_usage_machine  ON usage(machine_id, ts);
`

// Insert dedup-inserts a batch using ON CONFLICT DO NOTHING and reports
// (accepted, duplicates). One Tx; pgx CopyFrom would be faster for large
// batches but we want to know exact accepted-vs-duplicate counts.
func (s *Store) Insert(ctx context.Context, machineID, userID string, recs []types.UsageRecord) (int, int, error) {
	if len(recs) == 0 {
		return 0, 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	const stmt = `INSERT INTO usage
        (message_id, request_id, session_id, user_id, machine_id, model, ts,
         input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
         project_path, backfill, received_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
        ON CONFLICT (message_id, request_id) DO NOTHING`

	batch := &pgx.Batch{}
	now := time.Now()
	considered := 0
	for _, r := range recs {
		if r.MessageID == "" {
			continue
		}
		considered++
		batch.Queue(stmt,
			r.MessageID, r.RequestID, r.SessionID, userID, machineID, r.Model,
			r.Timestamp,
			r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens,
			nullableText(r.ProjectPath), r.Backfill, now)
	}
	if considered == 0 {
		return 0, 0, nil
	}
	br := tx.SendBatch(ctx, batch)
	accepted := 0
	for i := 0; i < considered; i++ {
		ct, err := br.Exec()
		if err != nil {
			br.Close()
			return 0, 0, err
		}
		if ct.RowsAffected() > 0 {
			accepted++
		}
	}
	if err := br.Close(); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return accepted, considered - accepted, nil
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
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

// Aggregate returns per-day per-user per-model token totals (UTC days).
// Empty user filters off the user constraint; zero from/to does the same for time.
func (s *Store) Aggregate(ctx context.Context, user string, from, to time.Time) ([]AggRow, error) {
	q := `SELECT to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
                 user_id, model,
                 SUM(input_tokens), SUM(output_tokens),
                 SUM(cache_creation_tokens), SUM(cache_read_tokens),
                 COUNT(*)
          FROM usage
          WHERE ($1 = '' OR user_id = $1)
            AND ($2::timestamptz IS NULL OR ts >= $2)
            AND ($3::timestamptz IS NULL OR ts <  $3)
          GROUP BY day, user_id, model
          ORDER BY day DESC, user_id, model`
	var fromArg, toArg any
	if !from.IsZero() {
		fromArg = from
	}
	if !to.IsZero() {
		toArg = to
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
