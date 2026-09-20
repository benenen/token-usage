package parsers

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// One usage line, one Edit-tool result, one Write(create) result, one
// string toolUseResult (errored tool call — must be ignored), one
// unrelated user line.
const claudeCodeFixture = `{"type":"assistant","sessionId":"s1","timestamp":"2026-07-01T10:00:00Z","requestId":"req_1","message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":1,"cache_read_input_tokens":2}}}
{"type":"user","sessionId":"s1","uuid":"uuid-edit-1","timestamp":"2026-07-01T10:00:05Z","toolUseResult":{"filePath":"/w/proj/main.go","oldString":"a","newString":"b","structuredPatch":[{"oldStart":1,"oldLines":2,"newStart":1,"newLines":3,"lines":[" ctx","+added one","+added two","-removed"]}]}}
{"type":"user","sessionId":"s1","uuid":"uuid-write-1","timestamp":"2026-07-01T10:00:10Z","toolUseResult":{"type":"create","filePath":"/w/proj/README.md","content":"hello\nworld","structuredPatch":[],"originalFile":null,"userModified":false}}
{"type":"user","sessionId":"s1","uuid":"uuid-err-1","timestamp":"2026-07-01T10:00:15Z","toolUseResult":"Error: file has not been read yet"}
{"type":"user","sessionId":"s1","uuid":"uuid-msg-1","timestamp":"2026-07-01T10:00:20Z","message":{"role":"user","content":"hi"}}
`

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "-w-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeCodeScanEmitsUsageAndEdits(t *testing.T) {
	path := writeFixture(t, "session.jsonl", claudeCodeFixture)
	now := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)

	res, state, err := claudeCodeParser{}.Scan(path, "claude-code", FileState{}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage) != 1 {
		t.Fatalf("usage records = %d, want 1", len(res.Usage))
	}
	if res.Usage[0].MessageID != "msg_1" {
		t.Errorf("usage MessageID = %q", res.Usage[0].MessageID)
	}

	if len(res.Edits) != 2 {
		t.Fatalf("edit records = %d, want 2 (got %+v)", len(res.Edits), res.Edits)
	}
	e := res.Edits[0]
	if e.EventID != "uuid-edit-1" || e.SessionID != "s1" || e.Tool != "claude-code" {
		t.Errorf("edit identity fields wrong: %+v", e)
	}
	if e.Lang != "golang" || e.LinesAdded != 2 || e.LinesRemoved != 1 {
		t.Errorf("edit stats = lang=%s +%d/-%d, want golang +2/-1", e.Lang, e.LinesAdded, e.LinesRemoved)
	}
	if e.ProjectPath != "-w-proj" {
		t.Errorf("edit ProjectPath = %q, want -w-proj", e.ProjectPath)
	}
	w := res.Edits[1]
	if w.EventID != "uuid-write-1" || w.Lang != "markdown" || w.LinesAdded != 2 || w.LinesRemoved != 0 {
		t.Errorf("write stats wrong: %+v", w)
	}

	// Incremental re-scan from the returned state finds nothing new.
	res2, _, err := claudeCodeParser{}.Scan(path, "claude-code", state, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Usage) != 0 || len(res2.Edits) != 0 {
		t.Errorf("re-scan produced %d usage + %d edits, want 0 + 0", len(res2.Usage), len(res2.Edits))
	}
}

func TestClaudeCodeEditBackfillCutoff(t *testing.T) {
	path := writeFixture(t, "session.jsonl", claudeCodeFixture)
	// Records are stamped 2026-07-01; scanning from far in the future with
	// a 1h cutoff must mark them backfill.
	now := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	res, _, err := claudeCodeParser{}.Scan(path, "claude-code", FileState{}, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Edits {
		if !e.Backfill {
			t.Errorf("edit %s not marked backfill", e.EventID)
		}
	}
}

// The 1h subset drives the cache-write premium, so it must survive the
// parse, stay within the reported total, and be 0 when absent.
func TestClaudeCodeSplitsOneHourCacheCreation(t *testing.T) {
	const fixture = `{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":"2026-07-01T10:00:00.000Z","message":{"id":"msg_1h","model":"claude-opus-4-1","usage":{"input_tokens":2,"output_tokens":10,"cache_creation_input_tokens":1000,"cache_read_input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":900,"ephemeral_5m_input_tokens":100}}}}
{"type":"assistant","sessionId":"s1","requestId":"r2","timestamp":"2026-07-01T10:00:01.000Z","message":{"id":"msg_5m","model":"claude-opus-4-1","usage":{"input_tokens":2,"output_tokens":10,"cache_creation_input_tokens":700,"cache_read_input_tokens":5}}}
{"type":"assistant","sessionId":"s1","requestId":"r3","timestamp":"2026-07-01T10:00:02.000Z","message":{"id":"msg_odd","model":"claude-opus-4-1","usage":{"input_tokens":2,"output_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":999}}}}
`
	path := writeFixture(t, "cache.jsonl", fixture)
	res, _, err := claudeCodeParser{}.Scan(path, "claude-code", FileState{}, 0, time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage) != 3 {
		t.Fatalf("usage records = %d, want 3", len(res.Usage))
	}
	if got := res.Usage[0]; got.CacheCreationTokens != 1000 || got.CacheCreation1hTokens != 900 {
		t.Errorf("split record = cc %d / 1h %d, want 1000 / 900", got.CacheCreationTokens, got.CacheCreation1hTokens)
	}
	if got := res.Usage[1]; got.CacheCreationTokens != 700 || got.CacheCreation1hTokens != 0 {
		t.Errorf("no-detail record = cc %d / 1h %d, want 700 / 0", got.CacheCreationTokens, got.CacheCreation1hTokens)
	}
	if got := res.Usage[2]; got.CacheCreation1hTokens != 100 {
		t.Errorf("1h %d exceeds the cache-creation total; want it clamped to 100", got.CacheCreation1hTokens)
	}
}
