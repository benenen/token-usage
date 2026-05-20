package watcher

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tokenusage/internal/types"
)

// codexParser handles OpenAI codex CLI rollout sessions. Sessions are
// small single-author files; we treat them as immutable-once-finalized
// and re-parse on size/inode change rather than tailing by offset.
type codexParser struct{}

func init() { RegisterParser("codex", codexParser{}) }

func (codexParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) ([]types.UsageRecord, FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, FileState{}, err
	}
	inode := inodeOf(info)
	size := info.Size()
	if prev.Inode == inode && prev.Offset == size {
		return nil, FileState{Inode: inode, Offset: size}, nil
	}
	recs, err := parseCodexFile(path, tool, backfillCutoff, now)
	return recs, FileState{Inode: inode, Offset: size}, err
}

// Codex (OpenAI codex CLI) writes one session per file under
//   ~/.codex/sessions/YYYY/MM/DD/rollout-<datetime>-<uuid>.jsonl
//
// Each line is {timestamp, type, payload}. Token usage lives in
//   event_msg.payload.type == "token_count"
// where payload.info.total_token_usage is the *cumulative* running total
// for the session. payload.info.last_token_usage exists but Codex emits
// it duplicated across consecutive events, so summing it double-counts.
//
// We compute per-event deltas off total_token_usage. The model name is
// learned from turn_context events that appear before each call.
//
// Re-scan semantics: the file is small (single session ≤ ~1 MB) and may
// receive new lines as the session continues. The scanner re-reads the
// whole file whenever size/inode change; records are emitted with a
// stable synthetic message_id (cdx_<session>_<event-timestamp>) so the
// server's PK dedups them on every re-send.

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
	InputTokens          int64 `json:"input_tokens"`
	CachedInputTokens    int64 `json:"cached_input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// parseCodexFile reads a whole Codex rollout JSONL and returns one
// UsageRecord per actual API call (i.e. per non-zero delta of
// total_token_usage). Caller decides what to do with the result —
// the server dedups by (message_id, request_id) regardless.
func parseCodexFile(path, tool string, backfillCutoff time.Duration, now time.Time) ([]types.UsageRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)

	sessionID := sessionIDFromFilename(filepath.Base(path))
	model := ""
	project := projectFromPath(path)
	var prev codexTokens // zero-value start: first event's delta = its totals
	var out []types.UsageRecord

	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			emitCodexLine(line, &model, &sessionID, &prev, project, tool, backfillCutoff, now, &out)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return out, nil
			}
			return out, rerr
		}
	}
}

func emitCodexLine(
	line []byte,
	model *string,
	sessionID *string,
	prev *codexTokens,
	project, tool string,
	backfillCutoff time.Duration,
	now time.Time,
	out *[]types.UsageRecord,
) {
	var l codexLine
	if err := json.Unmarshal(line, &l); err != nil {
		return
	}
	switch l.Type {
	case "session_meta":
		// Use the meta id only if we couldn't derive one from the filename.
		if *sessionID == "" {
			var meta codexSessionMeta
			if json.Unmarshal(l.Payload, &meta) == nil {
				*sessionID = meta.ID
			}
		}
	case "turn_context":
		var tc codexTurnContext
		if json.Unmarshal(l.Payload, &tc) == nil && tc.Model != "" {
			*model = tc.Model
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
		// Delta over prev. If all zero, this event is a duplicate-emission
		// of the previous turn's totals — skip it.
		dIn := t.InputTokens - prev.InputTokens
		dOut := t.OutputTokens - prev.OutputTokens
		dCache := t.CachedInputTokens - prev.CachedInputTokens
		dReason := t.ReasoningOutputTokens - prev.ReasoningOutputTokens
		if dIn <= 0 && dOut <= 0 && dCache <= 0 && dReason <= 0 {
			return
		}
		// Guard against negative deltas (shouldn't happen but be defensive).
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
		*prev = *t

		ts, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		if ts.IsZero() {
			ts = now
		}
		backfill := backfillCutoff > 0 && now.Sub(ts) > backfillCutoff

		*out = append(*out, types.UsageRecord{
			MessageID:           "cdx_" + *sessionID + "_" + l.Timestamp,
			SessionID:           *sessionID,
			Tool:                tool,
			Model:               *model,
			Timestamp:           ts,
			InputTokens:         dIn,
			OutputTokens:        dOut + dReason, // count reasoning as output for cost
			CacheCreationTokens: 0,              // codex has no prompt-cache write phase
			CacheReadTokens:     dCache,
			ProjectPath:         project,
			Backfill:            backfill,
		})
	}
}

// rollout-2026-04-27T01-21-55-019dcc87-57a6-79e2-80ee-9a8c3b731c9b.jsonl
//                                       └────────────────── UUID ──────────┘
func sessionIDFromFilename(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	parts := strings.Split(base, "-")
	if len(parts) < 5 {
		return base
	}
	// UUID is the last 5 hyphen-separated groups.
	return strings.Join(parts[len(parts)-5:], "-")
}
