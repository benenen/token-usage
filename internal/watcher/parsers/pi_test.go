package parsers

import (
	"testing"
	"time"
)

// A pi session: leading session line + a model_change (both ignored), an
// assistant message that only produced text (usage, no edit), an assistant
// message whose usage-bearing turn also emitted a `write` toolCall, another
// whose turn emitted an `edit` toolCall, then a toolResult and a user
// message (neither carries usage or an edit). Line counts come from the
// toolCall arguments; the toolResult carries no diff, only isError.
const piFixture = `{"type":"session","version":3,"id":"019f87bf-a8d6-7990-bfd9-2b2c95c64dcb","timestamp":"2026-07-01T10:00:00.000Z","cwd":"/w/proj"}
{"type":"model_change","timestamp":"2026-07-01T10:00:00.500Z","model":"deepseek-v4-flash-free"}
{"type":"message","id":"aaaa1111","timestamp":"2026-07-01T10:00:01.000Z","message":{"role":"assistant","model":"deepseek-v4-flash-free","responseId":"resp-aaaa","content":[{"type":"thinking","thinking":"…"},{"type":"text","text":"hi"}],"usage":{"input":10,"output":20,"cacheRead":2,"cacheWrite":1,"reasoning":5,"totalTokens":30}}}
{"type":"message","id":"bbbb2222","timestamp":"2026-07-01T10:00:02.000Z","message":{"role":"assistant","model":"deepseek-v4-flash-free","responseId":"resp-bbbb","content":[{"type":"toolCall","id":"call_w","name":"write","arguments":{"path":"/w/proj/main.go","content":"package main\n\nfunc main() {}"}}],"usage":{"input":30,"output":5,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"cccc3333","timestamp":"2026-07-01T10:00:03.000Z","message":{"role":"assistant","model":"deepseek-v4-flash-free","content":[{"type":"toolCall","id":"call_e","name":"edit","arguments":{"path":"/w/proj/app.ts","edits":[{"oldText":"a\nb","newText":"x\ny\nz"}]}}],"usage":{"input":40,"output":8,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"tr1","timestamp":"2026-07-01T10:00:04.000Z","message":{"role":"toolResult","toolCallId":"call_w","toolName":"write","content":[{"type":"text","text":"Successfully wrote 32 bytes to /w/proj/main.go"}],"isError":false}}
{"type":"message","id":"uuuu4444","timestamp":"2026-07-01T10:00:05.000Z","message":{"role":"user","content":[{"type":"text","text":"thanks"}]}}
`

const piSessionUUID = "019f87bf-a8d6-7990-bfd9-2b2c95c64dcb"

func TestPiScanEmitsUsageAndEdits(t *testing.T) {
	path := writeFixture(t, "2026-07-01T10-00-00-000Z_"+piSessionUUID+".jsonl", piFixture)
	now := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)

	res, state, err := piParser{}.Scan(path, "pi", FileState{}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	// Three assistant messages carry usage; session / model_change /
	// toolResult / user lines must not.
	if len(res.Usage) != 3 {
		t.Fatalf("usage records = %d, want 3: %+v", len(res.Usage), res.Usage)
	}
	u := res.Usage[0]
	if u.MessageID != "pi_"+piSessionUUID+"_aaaa1111" {
		t.Errorf("MessageID = %q, want pi_<session>_aaaa1111", u.MessageID)
	}
	if u.RequestID != "resp-aaaa" {
		t.Errorf("RequestID = %q, want resp-aaaa (responseId)", u.RequestID)
	}
	if u.SessionID != piSessionUUID || u.Tool != "pi" || u.Model != "deepseek-v4-flash-free" {
		t.Errorf("identity fields wrong: %+v", u)
	}
	// pi usage field names: input/output/cacheRead/cacheWrite -> the four
	// token columns. reasoning is already folded into output, cost ignored.
	if u.InputTokens != 10 || u.OutputTokens != 20 || u.CacheCreationTokens != 1 || u.CacheReadTokens != 2 {
		t.Errorf("token mapping wrong: %+v", u)
	}
	if u.Timestamp.IsZero() {
		t.Error("timestamp not parsed")
	}

	// Two edits: the write (create-style, added only) and the edit
	// (oldText removed / newText added). The toolResult line is not one.
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
	if !ok || w.add != 3 || w.rem != 0 || w.id != "call_w" || w.sess != piSessionUUID {
		t.Errorf("write edit wrong: %+v", byLang["golang"])
	}
	ed, ok := byLang["typescript"]
	if !ok || ed.add != 3 || ed.rem != 2 || ed.id != "call_e" {
		t.Errorf("edit edit wrong: %+v", byLang["typescript"])
	}

	// Offset advanced to EOF so a re-scan yields nothing.
	res2, _, err := piParser{}.Scan(path, "pi", state, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Usage) != 0 || len(res2.Edits) != 0 {
		t.Errorf("re-scan from advanced state should be empty: %+v", res2)
	}
}

func TestPiSessionIDFromFilename(t *testing.T) {
	got := piSessionIDFromFilename("2026-07-01T10-00-00-000Z_" + piSessionUUID + ".jsonl")
	if got != piSessionUUID {
		t.Errorf("piSessionIDFromFilename = %q, want %q", got, piSessionUUID)
	}
	// No underscore -> fall back to the whole base name.
	if got := piSessionIDFromFilename("weird.jsonl"); got != "weird" {
		t.Errorf("fallback = %q, want weird", got)
	}
}
