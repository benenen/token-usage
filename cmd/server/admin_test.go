package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	serverstore "tokenusage/internal/server"
	"tokenusage/internal/testpostgres"
	"tokenusage/internal/types"
)

func TestMain(m *testing.M) {
	os.Exit(testpostgres.Run(m))
}

func TestCodexRepairCLIRequiresScopeFlags(t *testing.T) {
	result := runCodexRepairCLI(t, "admin", "codex-repair")
	if result.err == nil {
		t.Fatalf("error = nil, want required-flag error; output:\n%s", result.output)
	}
	if !strings.Contains(result.err.Error(), "--sessions, --user, and --machine are required") {
		t.Fatalf("output missing required-flag error:\n%s", result.output)
	}
}

func TestCodexRepairCLIDryRunOutput(t *testing.T) {
	resetCodexRepairDatabase(t)
	ts := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	insertCodexUsage(t, brokenCodexUsage("cdx_019dcc87-57a6-79e2-80ee-9a8c3b731c9b_2026-08-04T10:00:00.000Z", "session-one", ts))
	sessions := writeCodexSession(t, "019dcc87-57a6-79e2-80ee-9a8c3b731c9b", "2026-08-04T10:00:00.000Z", 100, 60, 30, 5)

	result := runCodexRepairCLI(t,
		"admin", "codex-repair",
		"--sessions", sessions,
		"--user", "user",
		"--machine", "machine",
	)
	if result.err != nil {
		t.Fatalf("execute dry-run: %v; output:\n%s", result.err, result.output)
	}
	assertOutputContains(t, result.output,
		"DRY-RUN parsed=1 database=1 matched=1 changed=1 missing_source=0 missing_db=0",
		"tokens input 100 -> 40, output 35 -> 30, cache_read 60",
		"dry-run only; re-run with --apply after taking a database backup",
	)
	assertStoredCodexTokens(t, "cdx_019dcc87-57a6-79e2-80ee-9a8c3b731c9b_2026-08-04T10:00:00.000Z", 100, 35, 60)
}

func TestCodexRepairCLIApplyIncompleteSourceAborts(t *testing.T) {
	resetCodexRepairDatabase(t)
	ts := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	firstID := "cdx_019dcc87-57a6-79e2-80ee-9a8c3b731c9b_2026-08-04T10:00:00.000Z"
	insertCodexUsage(t,
		brokenCodexUsage(firstID, "session-one", ts),
		brokenCodexUsage("cdx_missing_source", "session-two", ts.Add(time.Second)),
	)
	sessions := writeCodexSession(t, "019dcc87-57a6-79e2-80ee-9a8c3b731c9b", "2026-08-04T10:00:00.000Z", 100, 60, 30, 5)

	result := runCodexRepairCLI(t,
		"admin", "codex-repair",
		"--sessions", sessions,
		"--user", "user",
		"--machine", "machine",
		"--apply",
	)
	if result.err == nil {
		t.Fatalf("error = nil, want incomplete-source error; output:\n%s", result.output)
	}
	assertOutputContains(t, result.output,
		"ABORTED parsed=1 database=2 matched=1 changed=1 missing_source=1 missing_db=0",
	)
	if !strings.Contains(result.err.Error(), "repair source is incomplete; no changes were applied") {
		t.Fatalf("unexpected error: %v", result.err)
	}
	assertStoredCodexTokens(t, firstID, 100, 35, 60)
}

func TestCodexRepairCLIApplyAndIdempotence(t *testing.T) {
	resetCodexRepairDatabase(t)
	ts := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	messageID := "cdx_019dcc87-57a6-79e2-80ee-9a8c3b731c9b_2026-08-04T10:00:00.000Z"
	insertCodexUsage(t, brokenCodexUsage(messageID, "session-one", ts))
	sessions := writeCodexSession(t, "019dcc87-57a6-79e2-80ee-9a8c3b731c9b", "2026-08-04T10:00:00.000Z", 100, 60, 30, 5)

	applied := runCodexRepairCLI(t,
		"admin", "codex-repair",
		"--sessions", sessions,
		"--user", "user",
		"--machine", "machine",
		"--apply",
	)
	if applied.err != nil {
		t.Fatalf("execute apply: %v; output:\n%s", applied.err, applied.output)
	}
	assertOutputContains(t, applied.output,
		"APPLIED parsed=1 database=1 matched=1 changed=1 missing_source=0 missing_db=0",
	)
	assertStoredCodexTokens(t, messageID, 40, 30, 60)

	again := runCodexRepairCLI(t,
		"admin", "codex-repair",
		"--sessions", sessions,
		"--user", "user",
		"--machine", "machine",
		"--apply",
	)
	if again.err != nil {
		t.Fatalf("execute idempotent apply: %v; output:\n%s", again.err, again.output)
	}
	assertOutputContains(t, again.output,
		"NO-CHANGES parsed=1 database=1 matched=1 changed=0 missing_source=0 missing_db=0",
	)
	if strings.Contains(again.output, "dry-run only") {
		t.Fatalf("apply output unexpectedly contains dry-run notice:\n%s", again.output)
	}
}

type cliResult struct {
	err    error
	output string
}

func runCodexRepairCLI(t *testing.T, args ...string) cliResult {
	t.Helper()
	testDSN := os.Getenv("TOKENUSAGE_TEST_DSN")
	if testDSN == "" {
		t.Fatal("TOKENUSAGE_TEST_DSN was not provisioned by TestMain")
	}
	dsn = ""
	cmd := newRootCmd()
	cmd.SilenceErrors = true
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(append([]string{"--dsn", testDSN}, args...))
	return cliResult{err: cmd.ExecuteContext(context.Background()), output: output.String()}
}

func resetCodexRepairDatabase(t *testing.T) {
	t.Helper()
	store, err := serverstore.NewStore(context.Background(), os.Getenv("TOKENUSAGE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	pool := openTestPool(t)
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), `
		TRUNCATE usage_detail, usage_daily, edit_detail, edit_daily,
		         api_keys, users, model_prices CASCADE
	`); err != nil {
		t.Fatal(err)
	}
}

func insertCodexUsage(t *testing.T, rows ...types.UsageRecord) {
	t.Helper()
	store, err := serverstore.NewStore(context.Background(), os.Getenv("TOKENUSAGE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accepted, _, err := store.Insert(context.Background(), "machine", "user", rows)
	if err != nil || accepted != len(rows) {
		t.Fatalf("insert usage: accepted=%d want=%d err=%v", accepted, len(rows), err)
	}
}

func brokenCodexUsage(messageID, sessionID string, ts time.Time) types.UsageRecord {
	return types.UsageRecord{
		MessageID:       messageID,
		SessionID:       sessionID,
		Tool:            "codex",
		Model:           "gpt-5.6-sol",
		Timestamp:       ts,
		InputTokens:     100,
		OutputTokens:    35,
		CacheReadTokens: 60,
	}
}

func writeCodexSession(t *testing.T, sessionID, timestamp string, input, cached, output, reasoning int64) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "rollout-2026-08-04T10-00-00-"+sessionID+".jsonl")
	fixture := fmt.Sprintf(`{"timestamp":"%s","type":"session_meta","payload":{"id":"%s"}}
{"timestamp":"%s","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}
{"timestamp":"%s","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d}}}}
`, timestamp, sessionID, timestamp, timestamp, input, cached, output, reasoning)
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertStoredCodexTokens(t *testing.T, messageID string, wantInput, wantOutput, wantCache int64) {
	t.Helper()
	pool := openTestPool(t)
	defer pool.Close()
	var input, output, cache int64
	if err := pool.QueryRow(context.Background(), `
		SELECT input_tokens, output_tokens, cache_read_tokens
		FROM usage_detail WHERE message_id=$1 AND request_id=''
	`, messageID).Scan(&input, &output, &cache); err != nil {
		t.Fatal(err)
	}
	if input != wantInput || output != wantOutput || cache != wantCache {
		t.Fatalf("stored tokens = %d/%d/%d, want %d/%d/%d", input, output, cache, wantInput, wantOutput, wantCache)
	}
}

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("TOKENUSAGE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func assertOutputContains(t *testing.T, output string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(output, fragment) {
			t.Errorf("output missing %q:\n%s", fragment, output)
		}
	}
}
