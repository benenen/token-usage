package parsers

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// A dsh session log: the leading `session` header, an assistant message
// with usage that also issued a `write` (succeeded) and an `edit` (failed),
// a `bash` call (not an edit), and an interrupted step with no usage. Only
// the successful write becomes an EditRecord.
const dshFixture = `{"type":"session","version":4,"id":"session-8f81","createdAt":1792763628992,"cwd":"/w/proj","isSeeded":false}
{"type":"turn/start","seq":4,"time":1792763638105,"data":{"turn":1}}
{"type":"assistant/message","seq":20,"time":1792763640000,"data":{"turn":1,"step":1,"message":{"id":"m-1","role":"assistant","content":[],"source":{"kind":"model","provider":"deepseek-official","model":"deepseek-flash"}},"stream":[],"usage":{"inputTokens":100,"outputTokens":40,"totalTokens":1140,"cacheReadTokens":1000,"reasoningTokens":12}}}
{"type":"tool/call","seq":21,"time":1792763640100,"data":{"turn":1,"step":1,"callId":"call_w","name":"write","arguments":"{\"file_path\":\"/w/proj/main.go\",\"content\":\"package main\\n\\nfunc main() {}\\n\"}"}}
{"type":"tool/call","seq":22,"time":1792763640200,"data":{"turn":1,"step":1,"callId":"call_e","name":"edit","arguments":"{\"file_path\":\"/w/proj/a.ts\",\"old_string\":\"a\",\"new_string\":\"b\"}"}}
{"type":"tool/call","seq":23,"time":1792763640300,"data":{"turn":1,"step":1,"callId":"call_b","name":"bash","arguments":"{\"command\":\"ls\"}"}}
{"type":"tool/result","seq":24,"time":1792763640400,"data":{"turn":1,"step":1,"message":{"id":"r-1","role":"tool","content":[],"source":{"kind":"tool","callId":"call_w"},"toolCallId":"call_w"}}}
{"type":"tool/result","seq":25,"time":1792763640500,"data":{"turn":1,"step":1,"message":{"id":"r-2","role":"tool","content":[],"source":{"kind":"tool","callId":"call_e"},"toolCallId":"call_e","isError":true}}}
{"type":"tool/result","seq":26,"time":1792763640600,"data":{"turn":1,"step":1,"message":{"id":"r-3","role":"tool","content":[],"source":{"kind":"tool","callId":"call_b"},"toolCallId":"call_b"}}}
{"type":"assistant/message","seq":30,"time":1792763650000,"data":{"turn":1,"step":2,"message":{"id":"m-2","role":"assistant","content":[],"source":{"kind":"model","provider":"deepseek-official","model":"deepseek-flash"}},"stream":[],"interrupted":true}}
`

func writeZstdFixture(t *testing.T, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "--w-proj--", "session-8f81")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.v4.jsonl.zstd")
	if err := os.WriteFile(path, enc.EncodeAll([]byte(content), nil), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDeepseekHarnessScan(t *testing.T) {
	path := writeZstdFixture(t, dshFixture)
	now := time.UnixMilli(1792763700000)

	res, state, err := deepseekHarnessParser{}.Scan(path, "deepseek-harness", FileState{}, 0, now)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Usage) != 1 {
		t.Fatalf("usage records = %d, want 1: %+v", len(res.Usage), res.Usage)
	}
	u := res.Usage[0]
	if u.MessageID != "dsh_session-8f81_m-1" || u.SessionID != "session-8f81" || u.Tool != "deepseek-harness" || u.Model != "deepseek-flash" {
		t.Errorf("identity fields wrong: %+v", u)
	}
	// inputTokens excludes cache hits; reasoning is already inside output.
	if u.InputTokens != 100 || u.OutputTokens != 40 || u.CacheReadTokens != 1000 || u.CacheCreationTokens != 0 {
		t.Errorf("token mapping wrong: %+v", u)
	}
	if !u.Timestamp.Equal(time.UnixMilli(1792763640000)) || u.ProjectPath != "/w/proj" {
		t.Errorf("timestamp/project wrong: %+v", u)
	}

	if len(res.Edits) != 1 {
		t.Fatalf("edits = %d, want 1 (failed edit and bash excluded): %+v", len(res.Edits), res.Edits)
	}
	e := res.Edits[0]
	if e.EventID != "call_w" || e.Lang != "golang" || e.LinesAdded != 3 || e.LinesRemoved != 0 {
		t.Errorf("edit wrong: %+v", e)
	}

	// Unchanged file: no re-parse.
	res, _, err = deepseekHarnessParser{}.Scan(path, "deepseek-harness", state, 0, now)
	if err != nil || len(res.Usage) != 0 || len(res.Edits) != 0 {
		t.Errorf("unchanged rescan = %+v, %v; want empty", res, err)
	}
}

func TestDeepseekHarnessScanTruncatedFrame(t *testing.T) {
	path := writeZstdFixture(t, dshFixture)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b[:len(b)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (deepseekHarnessParser{}).Scan(path, "deepseek-harness", FileState{}, 0, time.Now()); err == nil {
		t.Error("truncated zstd frame: want error so the checkpoint is not advanced")
	}
}
