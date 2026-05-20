package parsers

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

func (codexParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) ([]types.UsageRecord, FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, FileState{}, err
	}
	inode := inodeOf(info)
	size := info.Size()
	if prev.Inode == inode && prev.Offset == size {
		return nil, FileState{Inode: inode, Offset: size}, nil // unchanged
	}
	recs, err := parseCodexFile(path, tool, backfillCutoff, now)
	return recs, FileState{Inode: inode, Offset: size}, err
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
	var prev codexTokens
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
		dIn := t.InputTokens - prev.InputTokens
		dOut := t.OutputTokens - prev.OutputTokens
		dCache := t.CachedInputTokens - prev.CachedInputTokens
		dReason := t.ReasoningOutputTokens - prev.ReasoningOutputTokens
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
			OutputTokens:        dOut + dReason, // reasoning is billable output
			CacheCreationTokens: 0,              // codex has no separate cache write
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
	return strings.Join(parts[len(parts)-5:], "-")
}
