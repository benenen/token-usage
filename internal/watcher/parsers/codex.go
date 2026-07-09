package parsers

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tokenusage/internal/types"
)

// codexParser handles OpenAI codex CLI rollout sessions. Sessions live at
//   ~/.codex/sessions/YYYY/MM/DD/rollout-<datetime>-<uuid>.jsonl
//
// Each line is {timestamp, type, payload}. Token usage lives in
//   event_msg.payload.type == "token_count"
// where payload.info.total_token_usage is the *cumulative* running total
// for the session. payload.info.last_token_usage exists too but Codex
// emits it duplicated across consecutive events, so summing it double-
// counts. We compute per-event deltas off total_token_usage instead.
//
// Session files are small (~100 KB) and may grow as the session continues.
// We re-parse the whole file whenever size/inode change; the server's PK
// dedups records on every re-send.
type codexParser struct{}

func init() { register("codex", codexParser{}) }

func (codexParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) (ScanResult, FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ScanResult{}, FileState{}, err
	}
	inode := inodeOf(info)
	size := info.Size()
	if prev.Inode == inode && prev.Offset == size {
		return ScanResult{}, FileState{Inode: inode, Offset: size}, nil // unchanged
	}
	res, err := parseCodexFile(path, tool, backfillCutoff, now)
	return res, FileState{Inode: inode, Offset: size}, err
}

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMeta struct {
	ID string `json:"id"`
}

type codexTurnContext struct {
	Model string `json:"model"`
}

type codexTokenCount struct {
	Type string `json:"type"` // "token_count"
	Info *struct {
		TotalTokenUsage *codexTokens `json:"total_token_usage"`
	} `json:"info"`
}

type codexTokens struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// codexPatchCall is one apply_patch custom_tool_call awaiting its output
// line. Codex writes the call and its custom_tool_call_output as separate
// JSONL lines (output later in the file); we only emit EditRecords for
// calls whose output confirms the patch actually applied (exit_code 0).
// Since this parser re-reads the whole file whenever it changes, a call
// whose output hasn't been flushed yet is simply picked up next tick.
type codexPatchCall struct {
	callID string
	ts     string
	files  []codexFilePatch
}

func parseCodexFile(path, tool string, backfillCutoff time.Duration, now time.Time) (ScanResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return ScanResult{}, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)

	st := &codexScanState{
		sessionID: sessionIDFromFilename(filepath.Base(path)),
		project:   projectFromPath(path),
		patchOK:   make(map[string]bool),
	}

	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			emitCodexLine(line, st, tool, backfillCutoff, now)
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return st.result(tool, backfillCutoff, now), rerr
			}
			return st.result(tool, backfillCutoff, now), nil
		}
	}
}

// codexScanState accumulates per-file parse state across lines.
type codexScanState struct {
	sessionID string
	model     string
	project   string
	prev      codexTokens
	usage     []types.UsageRecord
	patches   []codexPatchCall
	patchOK   map[string]bool
}

// result assembles the final ScanResult, emitting one EditRecord per
// file of every apply_patch whose output confirmed success.
func (st *codexScanState) result(tool string, backfillCutoff time.Duration, now time.Time) ScanResult {
	res := ScanResult{Usage: st.usage}
	for _, p := range st.patches {
		if !st.patchOK[p.callID] {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, p.ts)
		if ts.IsZero() {
			ts = now
		}
		for i, fp := range p.files {
			res.Edits = append(res.Edits, types.EditRecord{
				EventID:      p.callID + "#" + strconv.Itoa(i),
				SessionID:    st.sessionID,
				Tool:         tool,
				Timestamp:    ts,
				Lang:         langFromPath(fp.path),
				LinesAdded:   fp.added,
				LinesRemoved: fp.removed,
				ProjectPath:  st.project,
				Backfill:     backfillCutoff > 0 && now.Sub(ts) > backfillCutoff,
			})
		}
	}
	return res
}

type codexToolCall struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
	Output string `json:"output"`
}

func emitCodexLine(line []byte, st *codexScanState, tool string, backfillCutoff time.Duration, now time.Time) {
	var l codexLine
	if err := json.Unmarshal(line, &l); err != nil {
		return
	}
	switch l.Type {
	case "session_meta":
		if st.sessionID == "" {
			var meta codexSessionMeta
			if json.Unmarshal(l.Payload, &meta) == nil {
				st.sessionID = meta.ID
			}
		}
	case "turn_context":
		var tc codexTurnContext
		if json.Unmarshal(l.Payload, &tc) == nil && tc.Model != "" {
			st.model = tc.Model
		}
	case "response_item":
		var call codexToolCall
		if json.Unmarshal(l.Payload, &call) != nil {
			return
		}
		switch call.Type {
		case "custom_tool_call":
			if call.Name != "apply_patch" || call.CallID == "" {
				return
			}
			if files := parseApplyPatch(call.Input); len(files) > 0 {
				st.patches = append(st.patches, codexPatchCall{callID: call.CallID, ts: l.Timestamp, files: files})
			}
		case "custom_tool_call_output":
			if call.CallID != "" && codexOutputSucceeded(call.Output) {
				st.patchOK[call.CallID] = true
			}
		}
	case "event_msg":
		var tc codexTokenCount
		if json.Unmarshal(l.Payload, &tc) != nil {
			return
		}
		if tc.Type != "token_count" || tc.Info == nil || tc.Info.TotalTokenUsage == nil {
			return
		}
		t := tc.Info.TotalTokenUsage
		dIn := t.InputTokens - st.prev.InputTokens
		dOut := t.OutputTokens - st.prev.OutputTokens
		dCache := t.CachedInputTokens - st.prev.CachedInputTokens
		dReason := t.ReasoningOutputTokens - st.prev.ReasoningOutputTokens
		if dIn <= 0 && dOut <= 0 && dCache <= 0 && dReason <= 0 {
			return // dup emission of the previous turn's totals
		}
		if dIn < 0 {
			dIn = 0
		}
		if dOut < 0 {
			dOut = 0
		}
		if dCache < 0 {
			dCache = 0
		}
		if dReason < 0 {
			dReason = 0
		}
		st.prev = *t

		ts, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		if ts.IsZero() {
			ts = now
		}
		backfill := backfillCutoff > 0 && now.Sub(ts) > backfillCutoff

		st.usage = append(st.usage, types.UsageRecord{
			MessageID:           "cdx_" + st.sessionID + "_" + l.Timestamp,
			SessionID:           st.sessionID,
			Tool:                tool,
			Model:               st.model,
			Timestamp:           ts,
			InputTokens:         dIn,
			OutputTokens:        dOut + dReason, // reasoning is billable output
			CacheCreationTokens: 0,              // codex has no separate cache write
			CacheReadTokens:     dCache,
			ProjectPath:         st.project,
			Backfill:            backfill,
		})
	}
}

// codexFilePatch is the per-file diffstat of one apply_patch section.
type codexFilePatch struct {
	path           string
	added, removed int64
}

// parseApplyPatch walks codex's apply_patch envelope format:
//
//	*** Begin Patch
//	*** Update File: <path>   (or Add File / Delete File)
//	@@ hunk header
//	-removed
//	+added
//	*** End Patch
//
// and returns per-file added/removed counts. Delete File sections carry
// no body, so they are skipped (nothing meaningful to count).
func parseApplyPatch(input string) []codexFilePatch {
	var out []codexFilePatch
	cur := -1 // index into out; -1 = not inside a counted section
	for _, l := range strings.Split(input, "\n") {
		switch {
		case strings.HasPrefix(l, "*** Update File: "), strings.HasPrefix(l, "*** Add File: "):
			path := strings.TrimSpace(l[strings.Index(l, ": ")+2:])
			out = append(out, codexFilePatch{path: path})
			cur = len(out) - 1
		case strings.HasPrefix(l, "*** "): // Delete File / End Patch / Move to / Begin Patch
			cur = -1
		case cur >= 0 && strings.HasPrefix(l, "+"):
			out[cur].added++
		case cur >= 0 && strings.HasPrefix(l, "-"):
			out[cur].removed++
		}
	}
	return out
}

// codexOutputSucceeded decides whether a custom_tool_call_output line
// reports a successfully applied patch. The output field is itself a
// JSON document {"output": "...", "metadata": {"exit_code": N}}; fall
// back to the "Success." prefix convention when that shape is absent.
func codexOutputSucceeded(output string) bool {
	var parsed struct {
		Output   string `json:"output"`
		Metadata struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(output), &parsed) == nil {
		if parsed.Metadata.ExitCode != nil {
			return *parsed.Metadata.ExitCode == 0
		}
		if parsed.Output != "" {
			return strings.HasPrefix(parsed.Output, "Success")
		}
	}
	return strings.HasPrefix(output, "Success")
}

// rollout-2026-04-27T01-21-55-019dcc87-57a6-79e2-80ee-9a8c3b731c9b.jsonl
//                                       └────────────────── UUID ──────────┘
func sessionIDFromFilename(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	parts := strings.Split(base, "-")
	if len(parts) < 5 {
		return base
	}
	return strings.Join(parts[len(parts)-5:], "-")
}
