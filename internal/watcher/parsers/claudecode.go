package parsers

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"tokenusage/internal/types"
)

// claudeCodeParser tails Anthropic Claude Code JSONL files incrementally.
// State is (inode, byte_offset); each tick only reads bytes appended since
// last checkpoint. Handles file rotation (inode change → restart from 0)
// and truncation (size < prev offset → restart from 0).
type claudeCodeParser struct{}

func init() { register("claude-code", claudeCodeParser{}) }

func (claudeCodeParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) (ScanResult, FileState, error) {
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
	project := projectFromPath(path)
	var res ScanResult
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			cur += int64(len(line))
			if rec, ok := parseClaudeCodeLine(line, project, tool, backfillCutoff, now); ok {
				res.Usage = append(res.Usage, rec)
			} else if edit, ok := parseClaudeCodeEditLine(line, project, tool, backfillCutoff, now); ok {
				res.Edits = append(res.Edits, edit)
			}
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

// rawLine mirrors the Claude Code JSONL line shape, only the fields we need.
type rawLine struct {
	Type          string          `json:"type"`
	SessionID     string          `json:"sessionId"`
	Cwd           string          `json:"cwd"`
	Timestamp     string          `json:"timestamp"`
	RequestID     string          `json:"requestId"`
	UUID          string          `json:"uuid"`
	Message       *rawMessage     `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
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

// rawToolResult mirrors the toolUseResult object of Edit / Write tool
// calls. Edit results carry a populated structuredPatch; Write results
// (type=create|update) carry the file content — for creates the
// structuredPatch is empty in practice, so line counts come from content.
type rawToolResult struct {
	Type            string `json:"type"` // "create" | "update" (Write); absent for Edit
	FilePath        string `json:"filePath"`
	Content         string `json:"content"`
	StructuredPatch []struct {
		Lines []string `json:"lines"`
	} `json:"structuredPatch"`
}

// parseClaudeCodeEditLine extracts an EditRecord from a tool-result line
// (type=="user" rows carrying a toolUseResult object). Errored tool
// calls have a plain-string toolUseResult and are skipped, as is any
// result without a filePath (reads, bash output, …).
func parseClaudeCodeEditLine(line []byte, project, tool string, backfillCutoff time.Duration, now time.Time) (types.EditRecord, bool) {
	var raw rawLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return types.EditRecord{}, false
	}
	if raw.Type != "user" || raw.UUID == "" || len(raw.ToolUseResult) == 0 || raw.ToolUseResult[0] != '{' {
		return types.EditRecord{}, false
	}
	var tr rawToolResult
	if err := json.Unmarshal(raw.ToolUseResult, &tr); err != nil {
		return types.EditRecord{}, false
	}
	if tr.FilePath == "" {
		return types.EditRecord{}, false
	}
	var added, removed int64
	switch {
	case len(tr.StructuredPatch) > 0: // Edit, or Write-update with a diff
		for _, hunk := range tr.StructuredPatch {
			a, r := diffLineCounts(hunk.Lines)
			added += a
			removed += r
		}
	case (tr.Type == "create" || tr.Type == "update") && tr.Content != "":
		// Write results ship no hunks — count the written content as
		// added. For updates this over-counts slightly (the replaced
		// lines aren't visible), same approximation as opencode writes.
		added = int64(strings.Count(strings.TrimSuffix(tr.Content, "\n"), "\n")) + 1
	default:
		return types.EditRecord{}, false
	}
	ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)
	if ts.IsZero() {
		ts = now
	}
	return types.EditRecord{
		EventID:      raw.UUID,
		SessionID:    raw.SessionID,
		Tool:         tool,
		Timestamp:    ts,
		Lang:         langFromPath(tr.FilePath),
		LinesAdded:   added,
		LinesRemoved: removed,
		ProjectPath:  project,
		Backfill:     backfillCutoff > 0 && now.Sub(ts) > backfillCutoff,
	}, true
}
