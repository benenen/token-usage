package server

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tokenusage/internal/testpostgres"
	"tokenusage/internal/types"
)

func TestMain(m *testing.M) {
	os.Exit(testpostgres.Run(m))
}

var repairTestMu sync.Mutex

func openRepairTestStore(t *testing.T) *Store {
	t.Helper()
	repairTestMu.Lock()
	dsn := os.Getenv("TOKENUSAGE_TEST_DSN")
	if dsn == "" {
		repairTestMu.Unlock()
		t.Fatal("TOKENUSAGE_TEST_DSN was not provisioned by TestMain")
	}
	store, err := NewStore(context.Background(), dsn)
	if err != nil {
		repairTestMu.Unlock()
		t.Fatal(err)
	}
	var databaseName string
	if err := store.pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&databaseName); err != nil {
		store.Close()
		repairTestMu.Unlock()
		t.Fatal(err)
	}
	if !strings.HasPrefix(databaseName, "tokenusage_") || !strings.Contains(databaseName, "_test_") {
		store.Close()
		repairTestMu.Unlock()
		t.Fatalf("refusing to truncate non-test database %q", databaseName)
	}
	if _, err := store.pool.Exec(context.Background(), `
		TRUNCATE usage_detail, usage_daily, edit_detail, edit_daily,
		         api_keys, users, model_prices CASCADE
	`); err != nil {
		store.Close()
		repairTestMu.Unlock()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		repairTestMu.Unlock()
	})
	return store
}

func TestRepairCodexUsageRequiresScope(t *testing.T) {
	store := openRepairTestStore(t)
	for _, scope := range []struct{ user, machine string }{{"", "machine"}, {"user", ""}} {
		if _, err := store.RepairCodexUsage(context.Background(), scope.user, scope.machine, nil, false); err == nil {
			t.Fatalf("RepairCodexUsage(%q, %q) error = nil", scope.user, scope.machine)
		}
	}
}

func TestRepairCodexUsageDryRunApplyAndIdempotence(t *testing.T) {
	store := openRepairTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	broken := types.UsageRecord{
		MessageID: "cdx_session_2026-08-04T10:00:00Z", SessionID: "session",
		Tool: "codex", Model: "gpt-5.6-sol", Timestamp: ts,
		InputTokens: 100, OutputTokens: 35, CacheReadTokens: 60,
	}
	if accepted, _, err := store.Insert(ctx, "machine", "user", []types.UsageRecord{broken}); err != nil || accepted != 1 {
		t.Fatalf("insert broken row: accepted=%d err=%v", accepted, err)
	}
	corrected := broken
	corrected.InputTokens = 40
	corrected.OutputTokens = 30

	dry, err := store.RepairCodexUsage(ctx, "user", "machine", []types.UsageRecord{corrected}, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Changed != 1 || dry.Applied || dry.MissingSource != 0 || dry.MissingDB != 0 {
		t.Fatalf("unexpected dry-run stats: %+v", dry)
	}
	assertStoredTokens(t, store, broken.MessageID, 100, 35, 60)

	applied, err := store.RepairCodexUsage(ctx, "user", "machine", []types.UsageRecord{corrected}, true)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Changed != 1 || !applied.Applied {
		t.Fatalf("unexpected apply stats: %+v", applied)
	}
	assertStoredTokens(t, store, broken.MessageID, 40, 30, 60)

	var dailyInput, dailyOutput, dailyCache int64
	if err := store.pool.QueryRow(ctx, `
		SELECT input_tokens, output_tokens, cache_read_tokens
		FROM usage_daily
		WHERE user_id='user' AND machine_id='machine' AND tool='codex'
	`).Scan(&dailyInput, &dailyOutput, &dailyCache); err != nil {
		t.Fatal(err)
	}
	if dailyInput != 40 || dailyOutput != 30 || dailyCache != 60 {
		t.Fatalf("daily tokens = %d/%d/%d, want 40/30/60", dailyInput, dailyOutput, dailyCache)
	}

	again, err := store.RepairCodexUsage(ctx, "user", "machine", []types.UsageRecord{corrected}, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed != 0 || again.Applied {
		t.Fatalf("repair is not idempotent: %+v", again)
	}
}

func TestRepairCodexUsageRejectsIncompleteSource(t *testing.T) {
	store := openRepairTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	rows := []types.UsageRecord{
		{MessageID: "cdx_one", SessionID: "one", Tool: "codex", Model: "gpt-5.6-sol", Timestamp: ts, InputTokens: 100, OutputTokens: 35, CacheReadTokens: 60},
		{MessageID: "cdx_two", SessionID: "two", Tool: "codex", Model: "gpt-5.6-sol", Timestamp: ts.Add(time.Second), InputTokens: 80, OutputTokens: 10, CacheReadTokens: 50},
	}
	if accepted, _, err := store.Insert(ctx, "machine", "user", rows); err != nil || accepted != 2 {
		t.Fatalf("insert rows: accepted=%d err=%v", accepted, err)
	}
	unchangedStats, err := store.RepairCodexUsage(ctx, "user", "machine", rows[:1], true)
	if err == nil {
		t.Fatal("expected incomplete source error even when matched rows need no changes")
	}
	if unchangedStats.MissingSource != 1 || unchangedStats.Changed != 0 || unchangedStats.Applied {
		t.Fatalf("unexpected unchanged incomplete-source stats: %+v", unchangedStats)
	}
	corrected := rows[0]
	corrected.InputTokens = 40
	corrected.OutputTokens = 30

	stats, err := store.RepairCodexUsage(ctx, "user", "machine", []types.UsageRecord{corrected}, true)
	if err == nil {
		t.Fatal("expected incomplete source error")
	}
	if stats.MissingSource != 1 || stats.Changed != 1 || stats.Applied {
		t.Fatalf("unexpected incomplete-source stats: %+v", stats)
	}
	assertStoredTokens(t, store, rows[0].MessageID, 100, 35, 60)
}

func TestRepairCodexUsageReportsSourceRowsMissingFromDatabase(t *testing.T) {
	store := openRepairTestStore(t)
	ctx := context.Background()
	ts := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	broken := types.UsageRecord{
		MessageID: "cdx_present", SessionID: "session", Tool: "codex", Model: "gpt-5.6-sol", Timestamp: ts,
		InputTokens: 100, OutputTokens: 35, CacheReadTokens: 60,
	}
	if accepted, _, err := store.Insert(ctx, "machine", "user", []types.UsageRecord{broken}); err != nil || accepted != 1 {
		t.Fatalf("insert broken row: accepted=%d err=%v", accepted, err)
	}
	corrected := broken
	corrected.InputTokens = 40
	corrected.OutputTokens = 30
	extra := corrected
	extra.MessageID = "cdx_not_ingested"

	stats, err := store.RepairCodexUsage(ctx, "user", "machine", []types.UsageRecord{corrected, extra}, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.MissingDB != 1 || stats.MissingSource != 0 || stats.Changed != 1 || !stats.Applied {
		t.Fatalf("unexpected missing-database stats: %+v", stats)
	}
	assertStoredTokens(t, store, broken.MessageID, 40, 30, 60)
}

func TestValidateCodexRepairRequiresChangedRange(t *testing.T) {
	if err := validateCodexRepair(CodexRepairStats{Changed: 1}); err == nil {
		t.Fatal("validateCodexRepair error = nil, want missing range error")
	}
}

func assertStoredTokens(t *testing.T, store *Store, messageID string, wantInput, wantOutput, wantCache int64) {
	t.Helper()
	var input, output, cache int64
	if err := store.pool.QueryRow(context.Background(), `
		SELECT input_tokens, output_tokens, cache_read_tokens
		FROM usage_detail WHERE message_id=$1 AND request_id=''
	`, messageID).Scan(&input, &output, &cache); err != nil {
		t.Fatal(err)
	}
	if input != wantInput || output != wantOutput || cache != wantCache {
		t.Fatalf("stored tokens = %d/%d/%d, want %d/%d/%d", input, output, cache, wantInput, wantOutput, wantCache)
	}
}
