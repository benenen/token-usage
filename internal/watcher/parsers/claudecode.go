package parsers

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"tokenusage/internal/types"
)

// claudeCodeParser tails Anthropic Claude Code JSONL files incrementally.
// State is (inode, byte_offset); each tick only reads bytes appended since
// last checkpoint. Handles file rotation (inode change → restart from 0)
// and truncation (size < prev offset → restart from 0).
type claudeCodeParser struct{}

func init() { register("claude-code", claudeCodeParser{}) }

func (claudeCodeParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) ([]types.UsageRecord, FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, FileState{}, err
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
		return nil, FileState{Inode: inode, Offset: offset}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, FileState{}, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, FileState{}, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	cur := offset
	project := projectFromPath(path)
	var recs []types.UsageRecord
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			cur += int64(len(line))
			if rec, ok := parseClaudeCodeLine(line, project, tool, backfillCutoff, now); ok {
				recs = append(recs, rec)
			}
		}
		if rerr != nil {
			// Partial trailing line (no '\n') stays for next tick; cur not advanced.
			if errors.Is(rerr, io.EOF) {
				break
			}
			return recs, FileState{Inode: inode, Offset: cur}, rerr
		}
	}
	return recs, FileState{Inode: inode, Offset: cur}, nil
}

// rawLine mirrors the Claude Code JSONL line shape, only the fields we need.
type rawLine struct {
	Type      string      `json:"type"`
	SessionID string      `json:"sessionId"`
	Cwd       string      `json:"cwd"`
	Timestamp string      `json:"timestamp"`
	RequestID string      `json:"requestId"`
	Message   *rawMessage `json:"message"`
}
type rawMessage struct {
	ID    string    `json:"id"`
	Model string    `json:"model"`
	Usage *rawUsage `json:"usage"`
}
type rawUsage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
}

func parseClaudeCodeLine(line []byte, project, tool string, backfillCutoff time.Duration, now time.Time) (types.UsageRecord, bool) {
	var raw rawLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return types.UsageRecord{}, false
	}
	if raw.Type != "assistant" || raw.Message == nil || raw.Message.Usage == nil {
		return types.UsageRecord{}, false
	}
	if raw.Message.ID == "" {
		return types.UsageRecord{}, false
	}
	// Locally-generated assistant messages (compaction summaries, replayed
	// tool results) carry model="<synthetic>" and never hit the API.
	if raw.Message.Model == "<synthetic>" {
		return types.UsageRecord{}, false
	}
	ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)
	if ts.IsZero() {
		ts = now
	}
	backfill := backfillCutoff > 0 && now.Sub(ts) > backfillCutoff
	return types.UsageRecord{
		MessageID:           raw.Message.ID,
		RequestID:           raw.RequestID,
		SessionID:           raw.SessionID,
		Tool:                tool,
		Model:               raw.Message.Model,
		Timestamp:           ts,
		InputTokens:         raw.Message.Usage.Input,
		OutputTokens:        raw.Message.Usage.Output,
		CacheCreationTokens: raw.Message.Usage.CacheCreation,
		CacheReadTokens:     raw.Message.Usage.CacheRead,
		ProjectPath:         project,
		Backfill:            backfill,
	}, true
}
