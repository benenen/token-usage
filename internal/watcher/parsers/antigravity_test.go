package parsers

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const antigravityConvID = "838435e3-b458-4fb1-bcb2-ff34712d1042"

// An Antigravity transcript: USER_INPUT (ignored), PLANNER_RESPONSE with usage
// and a read-only tool call, GENERIC tool result (ignored), PLANNER_RESPONSE
// with usage + write_file + edit_file, malformed JSON line (skipped without
// failing the file), and ERROR_MESSAGE (ignored).
const antigravityFixture = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T07:51:40Z","content":"hi"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T07:51:41Z","model":"gemini-4-neon-medium","input_tokens":11646,"cache_read_tokens":0,"output_tokens":250,"tool_calls":[{"name":"read_file","args":{"Path":"/w/proj/README.md"}}]}
{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-20T07:51:42Z","content":"readme"}
{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T07:51:50Z","model":"gemini-4-neon-medium","input_tokens":8465,"cache_read_tokens":11647,"cache_creation_tokens":100,"output_tokens":121,"tool_calls":[{"name":"write_file","args":{"Path":"/w/proj/main.go","Content":"package main\n\nfunc main() {}\n"}},{"name":"edit_file","args":{"Path":"/w/proj/app.ts","Target":"a\nb","Replacement":"x\ny\nz"}}]}
{not valid json
{"step_index":4,"source":"SYSTEM","type":"ERROR_MESSAGE","status":"DONE","created_at":"2026-11-20T07:51:55Z","error":"boom"}
`

func writeAntigravityFixture(t *testing.T, relPath, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), relPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAntigravityScanEmitsUsageAndEdits(t *testing.T) {
	path := writeAntigravityFixture(t, filepath.Join(antigravityConvID, ".system_generated", "logs", "transcript_full.jsonl"), antigravityFixture)
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

	res, state, err := antigravityParser{}.Scan(path, "antigravity", FileState{}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Usage) != 2 {
		t.Fatalf("usage records = %d, want 2: %+v", len(res.Usage), res.Usage)
	}
	u0 := res.Usage[0]
	if u0.MessageID != "antigravity_"+antigravityConvID+"_1" {
		t.Errorf("MessageID = %q, want antigravity_%s_1", u0.MessageID, antigravityConvID)
	}
	if u0.SessionID != antigravityConvID || u0.Tool != "antigravity" || u0.Model != "gemini-4-neon-medium" {
		t.Errorf("identity fields wrong: %+v", u0)
	}
	if u0.InputTokens != 11646 || u0.OutputTokens != 250 || u0.CacheReadTokens != 0 || u0.CacheCreationTokens != 0 {
		t.Errorf("u0 token mapping wrong: %+v", u0)
	}

	u1 := res.Usage[1]
	if u1.MessageID != "antigravity_"+antigravityConvID+"_3" {
		t.Errorf("u1 MessageID = %q, want antigravity_%s_3", u1.MessageID, antigravityConvID)
	}
	if u1.InputTokens != 8465 || u1.OutputTokens != 121 || u1.CacheReadTokens != 11647 || u1.CacheCreationTokens != 100 {
		t.Errorf("u1 token mapping wrong: %+v", u1)
	}

	if len(res.Edits) != 2 {
		t.Fatalf("edit records = %d, want 2: %+v", len(res.Edits), res.Edits)
	}
	byLang := map[string]struct {
		add, rem int64
		id, sess string
	}{}
	for _, e := range res.Edits {
		byLang[e.Lang] = struct {
			add, rem int64
			id, sess string
		}{e.LinesAdded, e.LinesRemoved, e.EventID, e.SessionID}
	}
	w, ok := byLang["golang"]
	if !ok || w.add != 3 || w.rem != 0 || w.id != "antigravity_"+antigravityConvID+"_3_0" || w.sess != antigravityConvID {
		t.Errorf("write_file edit wrong: %+v", byLang["golang"])
	}
	ed, ok := byLang["typescript"]
	if !ok || ed.add != 3 || ed.rem != 2 || ed.id != "antigravity_"+antigravityConvID+"_3_1" || ed.sess != antigravityConvID {
		t.Errorf("edit_file edit wrong: %+v", byLang["typescript"])
	}

	// Offset advanced to EOF so a re-scan yields nothing.
	res2, _, err := antigravityParser{}.Scan(path, "antigravity", state, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Usage) != 0 || len(res2.Edits) != 0 {
		t.Errorf("re-scan from advanced state should be empty: %+v", res2)
	}
}

func TestAntigravitySkipsCompactAndChunkTranscripts(t *testing.T) {
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	for _, rel := range []string{
		filepath.Join(antigravityConvID, ".system_generated", "logs", "transcript.jsonl"),
		filepath.Join(antigravityConvID, ".system_generated", "logs", "chunks", "transcript_full", "00000000.jsonl"),
	} {
		path := writeAntigravityFixture(t, rel, antigravityFixture)
		res, _, err := antigravityParser{}.Scan(path, "antigravity", FileState{}, 0, now)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", rel, err)
		}
		if len(res.Usage) != 0 || len(res.Edits) != 0 {
			t.Errorf("%s: expected skip, got %+v", rel, res)
		}
	}
}

func TestAntigravitySessionIDFromPath(t *testing.T) {
	p := filepath.Join("/home/u/.gemini/antigravity-cli/brain", antigravityConvID, ".system_generated", "logs", "transcript_full.jsonl")
	if got := antigravitySessionIDFromPath(p); got != antigravityConvID {
		t.Errorf("antigravitySessionIDFromPath = %q, want %q", got, antigravityConvID)
	}
	if got := antigravitySessionIDFromPath("/tmp/custom-dir/transcript_full.jsonl"); got != "custom-dir" {
		t.Errorf("fallback = %q, want custom-dir", got)
	}
}
