package parsers

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"tokenusage/internal/types"
)

// antigravityParser tails Antigravity CLI session transcripts incrementally.
// Antigravity stores per-conversation logs under
// ~/.gemini/antigravity-cli/brain/<conversation-id>/.system_generated/logs/:
//   - transcript.jsonl (compact, long tool_calls/content truncated)
//   - transcript_full.jsonl (complete, append-only, one JSON object per step)
//   - chunks/transcript{,_full}/*.jsonl (chunked copies)
//
// Only transcript_full.jsonl is parsed so we don't double-count the compact
// or chunked copies, and so write_file/edit_file tool_calls are never
// truncated. State is (inode, byte_offset) like claude-code and pi.
//
// Usage lives at top level on PLANNER_RESPONSE steps; input_tokens already
// excludes cache_read_tokens.
type antigravityParser struct{}

func init() { register("antigravity", antigravityParser{}) }

func (antigravityParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) (ScanResult, FileState, error) {
	if filepath.Base(path) != "transcript_full.jsonl" {
		return ScanResult{}, prev, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return ScanResult{}, FileState{}, err
	}
	inode := inodeOf(info)
	size := info.Size()

	offset := prev.Offset
	switch {
	case prev.Inode == 0 && prev.Offset == 0:
		offset = 0 // first time we see this file
	case prev.Inode != 0 && inode != 0 && prev.Inode != inode:
		offset = 0 // file rotated / replaced
	case size < offset:
		offset = 0 // file truncated
	}
	if size == offset {
		return ScanResult{}, FileState{Inode: inode, Offset: offset}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return ScanResult{}, FileState{}, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ScanResult{}, FileState{}, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	cur := offset
	sessionID := antigravitySessionIDFromPath(path)
	var res ScanResult
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			cur += int64(len(line))
			u, edits := parseAntigravityLine(line, tool, sessionID, backfillCutoff, now)
			if u != nil {
				res.Usage = append(res.Usage, *u)
			}
			res.Edits = append(res.Edits, edits...)
		}
		if rerr != nil {
			// Partial trailing line (no '\n') stays for next tick; cur not advanced.
			if errors.Is(rerr, io.EOF) {
				break
			}
			return res, FileState{Inode: inode, Offset: cur}, rerr
		}
	}
	return res, FileState{Inode: inode, Offset: cur}, nil
}

// antigravityLine mirrors one step line in Antigravity's transcript_full.jsonl.
type antigravityLine struct {
	StepIndex           *int                  `json:"step_index"`
	Type                string                `json:"type"`
	CreatedAt           string                `json:"created_at"`
	Model               string                `json:"model"`
	InputTokens         int64                 `json:"input_tokens"`
	OutputTokens        int64                 `json:"output_tokens"`
	CacheReadTokens     int64                 `json:"cache_read_tokens"`
	CacheCreationTokens int64                 `json:"cache_creation_tokens"`
	ToolCalls           []antigravityToolCall `json:"tool_calls"`
}

// antigravityToolCall keeps Args raw so non-edit tools with arbitrary argument
// shapes can never fail the line's unmarshal.
type antigravityToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type antigravityEditArgs struct {
	Path        string `json:"Path"`
	Content     string `json:"Content"`     // write_file
	Target      string `json:"Target"`      // edit_file
	Replacement string `json:"Replacement"` // edit_file
}

func parseAntigravityLine(line []byte, tool, sessionID string, backfillCutoff time.Duration, now time.Time) (*types.UsageRecord, []types.EditRecord) {
	var raw antigravityLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, nil
	}
	if raw.Type != "PLANNER_RESPONSE" || raw.StepIndex == nil {
		return nil, nil
	}
	ts, _ := time.Parse(time.RFC3339Nano, raw.CreatedAt)
	if ts.IsZero() {
		ts = now
	}
	backfill := backfillCutoff > 0 && now.Sub(ts) > backfillCutoff
	stepStr := strconv.Itoa(*raw.StepIndex)

	var usage *types.UsageRecord
	hasTokens := raw.InputTokens > 0 || raw.OutputTokens > 0 || raw.CacheReadTokens > 0 || raw.CacheCreationTokens > 0
	if raw.Model != "" && hasTokens {
		usage = &types.UsageRecord{
			MessageID:           "antigravity_" + sessionID + "_" + stepStr,
			SessionID:           sessionID,
			Tool:                tool,
			Model:               raw.Model,
			Timestamp:           ts,
			InputTokens:         raw.InputTokens,
			OutputTokens:        raw.OutputTokens,
			CacheCreationTokens: raw.CacheCreationTokens,
			CacheReadTokens:     raw.CacheReadTokens,
			Backfill:            backfill,
		}
	}

	var edits []types.EditRecord
	for i, tc := range raw.ToolCalls {
		if tc.Name != "write_file" && tc.Name != "edit_file" {
			continue
		}
		var args antigravityEditArgs
		if err := json.Unmarshal(tc.Args, &args); err != nil || args.Path == "" {
			continue
		}
		var added, removed int64
		if tc.Name == "write_file" {
			added = countLines(args.Content)
		} else {
			added = countLines(args.Replacement)
			removed = countLines(args.Target)
		}
		if added == 0 && removed == 0 {
			continue
		}
		edits = append(edits, types.EditRecord{
			EventID:      "antigravity_" + sessionID + "_" + stepStr + "_" + strconv.Itoa(i),
			SessionID:    sessionID,
			Tool:         tool,
			Timestamp:    ts,
			Lang:         langFromPath(args.Path),
			LinesAdded:   added,
			LinesRemoved: removed,
			Backfill:     backfill,
		})
	}
	return usage, edits
}

// antigravitySessionIDFromPath extracts the conversation UUID from
// .../<conversation-id>/.system_generated/logs/transcript_full.jsonl.
// Falls back to the file's parent directory name if the layout differs.
func antigravitySessionIDFromPath(p string) string {
	logsDir := filepath.Dir(p)
	sysGenDir := filepath.Dir(logsDir)
	if filepath.Base(logsDir) == "logs" && filepath.Base(sysGenDir) == ".system_generated" {
		if conv := filepath.Base(filepath.Dir(sysGenDir)); conv != "" && conv != "." && conv != string(filepath.Separator) {
			return conv
		}
	}
	return filepath.Base(logsDir)
}
