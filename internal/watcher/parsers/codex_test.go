package parsers

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseApplyPatch(t *testing.T) {
	input := "*** Begin Patch\n" +
		"*** Update File: /w/proj/a.go\n" +
		"@@\n" +
		"-old line\n" +
		"+new line\n" +
		"+extra line\n" +
		"*** Add File: /w/proj/b.rs\n" +
		"+fn main() {}\n" +
		"*** Delete File: /w/proj/c.txt\n" +
		"*** End Patch"

	files := parseApplyPatch(input)
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2 (deletes are skipped): %+v", len(files), files)
	}
	if files[0].path != "/w/proj/a.go" || files[0].added != 2 || files[0].removed != 1 {
		t.Errorf("file[0] = %+v, want /w/proj/a.go +2/-1", files[0])
	}
	if files[1].path != "/w/proj/b.rs" || files[1].added != 1 || files[1].removed != 0 {
		t.Errorf("file[1] = %+v, want /w/proj/b.rs +1/-0", files[1])
	}
}

// A successful apply_patch (exit_code 0), a failed one (exit_code 1),
// and a token_count event. Only the successful patch's files become
// EditRecords; usage still parses.
const codexFixture = `{"timestamp":"2026-07-01T10:00:00.000Z","type":"session_meta","payload":{"id":"sess-1"}}
{"timestamp":"2026-07-01T10:00:01.000Z","type":"turn_context","payload":{"model":"gpt-5.2-codex"}}
{"timestamp":"2026-07-01T10:00:02.000Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_ok","name":"apply_patch","input":"*** Begin Patch\n*** Update File: /w/proj/main.java\n@@\n-x\n+y\n+z\n*** End Patch"}}
{"timestamp":"2026-07-01T10:00:03.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_ok","output":"{\"output\":\"Success. Updated the following files:\\nM /w/proj/main.java\\n\",\"metadata\":{\"exit_code\":0,\"duration_seconds\":0.1}}"}}
{"timestamp":"2026-07-01T10:00:04.000Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_bad","name":"apply_patch","input":"*** Begin Patch\n*** Update File: /w/proj/broken.go\n@@\n-a\n+b\n*** End Patch"}}
{"timestamp":"2026-07-01T10:00:05.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_bad","output":"{\"output\":\"apply_patch: context mismatch\",\"metadata\":{\"exit_code\":1,\"duration_seconds\":0.1}}"}}
{"timestamp":"2026-07-01T10:00:06.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":30,"reasoning_output_tokens":5}}}}
`

func TestCodexScanEmitsEditsForSuccessfulPatches(t *testing.T) {
	path := writeFixture(t, "rollout-2026-07-01T10-00-00-019dcc87-57a6-79e2-80ee-9a8c3b731c9b.jsonl", codexFixture)
	now := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)

	res, _, err := codexParser{}.Scan(path, "codex", FileState{}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage) != 1 {
		t.Fatalf("usage records = %d, want 1", len(res.Usage))
	}
	if len(res.Edits) != 1 {
		t.Fatalf("edit records = %d, want 1 (failed patch must be skipped): %+v", len(res.Edits), res.Edits)
	}
	e := res.Edits[0]
	if e.EventID != "call_ok#0" {
		t.Errorf("EventID = %q, want call_ok#0", e.EventID)
	}
	if e.Lang != "java" || e.LinesAdded != 2 || e.LinesRemoved != 1 {
		t.Errorf("edit stats = lang=%s +%d/-%d, want java +2/-1", e.Lang, e.LinesAdded, e.LinesRemoved)
	}
	// Session ID follows the existing convention: derived from the rollout
	// filename's UUID (session_meta only fills in when that fails).
	if e.SessionID != "019dcc87-57a6-79e2-80ee-9a8c3b731c9b" || e.Tool != "codex" {
		t.Errorf("identity fields wrong: %+v", e)
	}
	if e.Timestamp.IsZero() {
		t.Error("timestamp not parsed")
	}
}

func TestCodexTokenUsageDoesNotDoubleCountIncludedBreakdowns(t *testing.T) {
	tests := []struct {
		name          string
		input         int64
		cachedInput   int64
		output        int64
		reasoning     int64
		wantInput     int64
		wantCacheRead int64
		wantOutput    int64
	}{
		{
			name:          "mixed cached input and reasoning output",
			input:         100,
			cachedInput:   60,
			output:        30,
			reasoning:     5,
			wantInput:     40,
			wantCacheRead: 60,
			wantOutput:    30,
		},
		{
			name:          "fully cached input",
			input:         100,
			cachedInput:   100,
			output:        20,
			reasoning:     20,
			wantInput:     0,
			wantCacheRead: 100,
			wantOutput:    20,
		},
		{
			name:          "no included breakdowns",
			input:         100,
			cachedInput:   0,
			output:        20,
			reasoning:     0,
			wantInput:     100,
			wantCacheRead: 0,
			wantOutput:    20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := fmt.Sprintf(`{"timestamp":"2026-07-01T10:00:00.000Z","type":"session_meta","payload":{"id":"sess-usage"}}
{"timestamp":"2026-07-01T10:00:01.000Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}
{"timestamp":"2026-07-01T10:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d}}}}
`, tt.input, tt.cachedInput, tt.output, tt.reasoning)
			path := writeFixture(t, "rollout-2026-07-01T10-00-00-019dcc87-57a6-79e2-80ee-9a8c3b731c9b.jsonl", fixture)

			res, _, err := codexParser{}.Scan(path, "codex", FileState{}, 0, time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Usage) != 1 {
				t.Fatalf("usage records = %d, want 1", len(res.Usage))
			}

			got := res.Usage[0]
			var mismatches []string
			if got.InputTokens != tt.wantInput {
				mismatches = append(mismatches, fmt.Sprintf("InputTokens = %d, want %d", got.InputTokens, tt.wantInput))
			}
			if got.CacheReadTokens != tt.wantCacheRead {
				mismatches = append(mismatches, fmt.Sprintf("CacheReadTokens = %d, want %d", got.CacheReadTokens, tt.wantCacheRead))
			}
			if got.OutputTokens != tt.wantOutput {
				mismatches = append(mismatches, fmt.Sprintf("OutputTokens = %d, want %d", got.OutputTokens, tt.wantOutput))
			}
			if len(mismatches) > 0 {
				t.Error(strings.Join(mismatches, "; "))
			}
		})
	}
}

func TestCodexTokenUsageUsesCumulativeDeltasAndSkipsDuplicates(t *testing.T) {
	fixture := `{"timestamp":"2026-07-01T10:00:00.000Z","type":"session_meta","payload":{"id":"sess-deltas"}}
{"timestamp":"2026-07-01T10:00:01.000Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}
{"timestamp":"2026-07-01T10:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":30,"reasoning_output_tokens":5}}}}
{"timestamp":"2026-07-01T10:00:03.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":30,"reasoning_output_tokens":5}}}}
{"timestamp":"2026-07-01T10:00:04.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":260,"cached_input_tokens":180,"output_tokens":70,"reasoning_output_tokens":15}}}}
`
	path := writeFixture(t, "rollout-2026-07-01T10-00-00-019dcc87-57a6-79e2-80ee-9a8c3b731c9b.jsonl", fixture)
	res, _, err := codexParser{}.Scan(path, "codex", FileState{}, 0, time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage) != 2 {
		t.Fatalf("usage records = %d, want 2", len(res.Usage))
	}
	wants := []struct{ input, output, cache int64 }{
		{input: 40, output: 30, cache: 60},
		{input: 40, output: 40, cache: 120},
	}
	for i, want := range wants {
		got := res.Usage[i]
		if got.InputTokens != want.input || got.OutputTokens != want.output || got.CacheReadTokens != want.cache {
			t.Errorf("usage[%d] = input/output/cache %d/%d/%d, want %d/%d/%d",
				i, got.InputTokens, got.OutputTokens, got.CacheReadTokens, want.input, want.output, want.cache)
		}
	}
}
