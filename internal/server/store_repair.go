package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"tokenusage/internal/types"
)

// CodexRepairStats describes one exact history reconciliation against
// corrected records replayed from Codex JSONL session files.
type CodexRepairStats struct {
	Parsed        int64
	DatabaseRows  int64
	Matched       int64
	Changed       int64
	MissingSource int64
	MissingDB     int64
	OldInput      int64
	NewInput      int64
	OldOutput     int64
	NewOutput     int64
	CacheRead     int64
	From          *time.Time
	To            *time.Time
	Applied       bool
}

// RepairCodexUsage reconciles already-ingested Codex rows with records
// replayed from the original JSONL files. It is a dry-run unless apply=true.
func (s *Store) RepairCodexUsage(ctx context.Context, userID, machineID string, records []types.UsageRecord, apply bool) (CodexRepairStats, error) {
	if userID == "" || machineID == "" {
		return CodexRepairStats{}, errors.New("user_id and machine_id are required")
	}
	payload, err := json.Marshal(records)
	if err != nil {
		return CodexRepairStats{}, err
	}
	tx, err := s.beginCodexRepair(ctx, apply)
	if err != nil {
		return CodexRepairStats{}, err
	}
	defer tx.Rollback(ctx)
	if err := stageCodexRepair(ctx, tx, payload); err != nil {
		return CodexRepairStats{}, err
	}
	stats, err := queryCodexRepairStats(ctx, tx, userID, machineID)
	if err != nil || !apply {
		return stats, err
	}
	if err := validateCodexRepair(stats); err != nil || stats.Changed == 0 {
		return stats, err
	}
	if err := applyCodexRepair(ctx, tx, userID, machineID, stats); err != nil {
		return stats, err
	}
	if err := tx.Commit(ctx); err != nil {
		return stats, err
	}
	stats.Applied = true
	return stats, nil
}

func (s *Store) beginCodexRepair(ctx context.Context, apply bool) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil || !apply {
		return tx, err
	}
	_, err = tx.Exec(ctx, `LOCK TABLE usage_detail, usage_daily IN SHARE ROW EXCLUSIVE MODE`)
	if err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func stageCodexRepair(ctx context.Context, tx pgx.Tx, payload []byte) error {
	if _, err := tx.Exec(ctx, createCodexRepairStageSQL); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, loadCodexRepairStageSQL, payload)
	return err
}

func queryCodexRepairStats(ctx context.Context, tx pgx.Tx, userID, machineID string) (CodexRepairStats, error) {
	var s CodexRepairStats
	err := tx.QueryRow(ctx, codexRepairStatsSQL, userID, machineID).Scan(
		&s.Parsed, &s.DatabaseRows, &s.Matched, &s.Changed,
		&s.MissingSource, &s.MissingDB,
		&s.OldInput, &s.NewInput, &s.OldOutput, &s.NewOutput,
		&s.CacheRead, &s.From, &s.To,
	)
	return s, err
}

func validateCodexRepair(stats CodexRepairStats) error {
	if stats.MissingSource > 0 {
		return errors.New("repair source is incomplete; no changes were applied")
	}
	if stats.Changed > 0 && (stats.From == nil || stats.To == nil) {
		return errors.New("changed rows have no timestamp range")
	}
	return nil
}

func applyCodexRepair(ctx context.Context, tx pgx.Tx, userID, machineID string, stats CodexRepairStats) error {
	if _, err := tx.Exec(ctx, updateCodexRepairSQL, userID, machineID); err != nil {
		return err
	}
	_, err := rebuildDailyTx(ctx, tx, *stats.From, *stats.To)
	return err
}

const createCodexRepairStageSQL = `
CREATE TEMP TABLE codex_repair_stage (
    message_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    input_tokens BIGINT NOT NULL,
    output_tokens BIGINT NOT NULL,
    cache_creation_tokens BIGINT NOT NULL,
    cache_read_tokens BIGINT NOT NULL,
    PRIMARY KEY (message_id, request_id)
) ON COMMIT DROP`

const loadCodexRepairStageSQL = `
WITH source AS (
    SELECT e, ordinality
    FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY AS x(e, ordinality)
)
INSERT INTO codex_repair_stage
    (message_id, request_id, input_tokens, output_tokens,
     cache_creation_tokens, cache_read_tokens)
SELECT COALESCE(e->>'message_id', ''), COALESCE(e->>'request_id', ''),
       COALESCE((e->>'input_tokens')::bigint, 0),
       COALESCE((e->>'output_tokens')::bigint, 0),
       COALESCE((e->>'cache_creation_tokens')::bigint, 0),
       COALESCE((e->>'cache_read_tokens')::bigint, 0)
FROM source
WHERE COALESCE(e->>'message_id', '') <> ''
ORDER BY ordinality
ON CONFLICT DO NOTHING`

const codexRepairStatsSQL = `
WITH scoped AS (
    SELECT d.* FROM usage_detail d
    WHERE d.tool = 'codex' AND d.user_id = $1 AND d.machine_id = $2
), matched AS (
    SELECT d.*, s.input_tokens AS new_input, s.output_tokens AS new_output,
           s.cache_creation_tokens AS new_cache_creation,
           s.cache_read_tokens AS new_cache_read
    FROM scoped d JOIN codex_repair_stage s USING (message_id, request_id)
), changed AS (
    SELECT * FROM matched
    WHERE input_tokens IS DISTINCT FROM new_input
       OR output_tokens IS DISTINCT FROM new_output
       OR cache_creation_tokens IS DISTINCT FROM new_cache_creation
       OR cache_read_tokens IS DISTINCT FROM new_cache_read
)
SELECT
    (SELECT COUNT(*) FROM codex_repair_stage),
    (SELECT COUNT(*) FROM scoped),
    (SELECT COUNT(*) FROM matched),
    (SELECT COUNT(*) FROM changed),
    (SELECT COUNT(*) FROM scoped d LEFT JOIN codex_repair_stage s USING (message_id, request_id) WHERE s.message_id IS NULL),
    (SELECT COUNT(*) FROM codex_repair_stage s LEFT JOIN scoped d USING (message_id, request_id) WHERE d.message_id IS NULL),
    COALESCE((SELECT SUM(input_tokens) FROM matched), 0),
    COALESCE((SELECT SUM(new_input) FROM matched), 0),
    COALESCE((SELECT SUM(output_tokens) FROM matched), 0),
    COALESCE((SELECT SUM(new_output) FROM matched), 0),
    COALESCE((SELECT SUM(new_cache_read) FROM matched), 0),
    (SELECT MIN(ts) FROM changed),
    (SELECT MAX(ts) FROM changed)`

const updateCodexRepairSQL = `
UPDATE usage_detail d
SET input_tokens = s.input_tokens,
    output_tokens = s.output_tokens,
    cache_creation_tokens = s.cache_creation_tokens,
    cache_read_tokens = s.cache_read_tokens
FROM codex_repair_stage s
WHERE d.message_id = s.message_id
  AND d.request_id = s.request_id
  AND d.tool = 'codex'
  AND d.user_id = $1
  AND d.machine_id = $2`
