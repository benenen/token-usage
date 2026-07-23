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

// piParser tails pi-agent session JSONL files incrementally, the same
// tail model as claudeCodeParser: state is (inode, byte_offset) and each
// tick only reads bytes appended since the last checkpoint. pi writes one
// file per session under ~/.pi/agent/sessions/<encoded-project>/<ts>_<uuid>.jsonl
// and appends a line per message, so the layout and rotation/truncation
// handling are identical to Claude Code's.
//
// Two things differ from Claude Code and shape this parser:
//   - Usage lives at message.usage with pi's own field names (input /
//     output / cacheRead / cacheWrite), and the model at message.model.
//   - A message line carries no sessionId; the session UUID is taken from
//     the filename (piSessionIDFromFilename), so it survives the fact that
//     a tail parser only ever reads the leading `session` line once.
type piParser struct{}

func init() { register("pi", piParser{}) }

func (piParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) (ScanResult, FileState, error) {
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
	sessionID := piSessionIDFromFilename(filepath.Base(path))
	var res ScanResult
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			cur += int64(len(line))
			// One assistant line can yield both a usage record and one or
			// more edit records (the usage-bearing turn is the same message
			// that emits the write/edit toolCall), so collect both.
			if rec, ok := parsePiUsageLine(line, project, tool, sessionID, backfillCutoff, now); ok {
				res.Usage = append(res.Usage, rec)
			}
			res.Edits = append(res.Edits, parsePiEditLines(line, project, tool, sessionID, backfillCutoff, now)...)
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

// piLine mirrors one pi JSONL line, only the fields we need. The per-line
// `id` is pi's message id (short, unique only within a session — hence the
// session-prefixed MessageID below); `timestamp` is the top-level ISO time.
type piLine struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	Timestamp string     `json:"timestamp"`
	Message   *piMessage `json:"message"`
}
type piMessage struct {
	Role       string    `json:"role"`
	Model      string    `json:"model"`
	ResponseID string    `json:"responseId"`
	Usage      *piUsage  `json:"usage"`
	Content    []piBlock `json:"content"`
}
type piUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

// piBlock is one content item. Arguments is kept raw so a non-object shape
// on some unrelated tool can never fail the whole line's unmarshal (which
// would also drop the usage record on the same line).
type piBlock struct {
	Type      string          `json:"type"` // "text" | "thinking" | "toolCall"
	ID        string          `json:"id"`   // toolCall id, e.g. "call_00_…"
	Name      string          `json:"name"` // tool name, e.g. "write" | "edit"
	Arguments json.RawMessage `json:"arguments"`
}
type piEditArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"` // write: full file body
	Edits   []struct {
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
	} `json:"edits"` // edit: one entry per replaced block
}

func parsePiUsageLine(line []byte, project, tool, sessionID string, backfillCutoff time.Duration, now time.Time) (types.UsageRecord, bool) {
	var raw piLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return types.UsageRecord{}, false
	}
	if raw.Type != "message" || raw.Message == nil || raw.Message.Role != "assistant" {
		return types.UsageRecord{}, false
	}
	if raw.Message.Usage == nil || raw.ID == "" || raw.Message.Model == "" {
		return types.UsageRecord{}, false
	}
	ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)
	if ts.IsZero() {
		ts = now
	}
	return types.UsageRecord{
		// pi's per-line id is unique only within a session; prefix the
		// session UUID (like codex's "cdx_…") so it can never collapse two
		// distinct rows under the (message_id, request_id) primary key.
		MessageID:           "pi_" + sessionID + "_" + raw.ID,
		RequestID:           raw.Message.ResponseID,
		SessionID:           sessionID,
		Tool:                tool,
		Model:               raw.Message.Model,
		Timestamp:           ts,
		InputTokens:         raw.Message.Usage.Input,
		OutputTokens:        raw.Message.Usage.Output, // already includes reasoning tokens
		CacheCreationTokens: raw.Message.Usage.CacheWrite,
		CacheReadTokens:     raw.Message.Usage.CacheRead,
		ProjectPath:         project,
		Backfill:            backfillCutoff > 0 && now.Sub(ts) > backfillCutoff,
	}, true
}

// parsePiEditLines turns any write/edit toolCall blocks on an assistant
// line into EditRecords. pi's toolResult carries no diff (only an isError
// flag and a "Successfully wrote N bytes"-style message), so line counts
// come from the toolCall arguments: write's `content` is counted as added,
// and each edit's oldText/newText as removed/added. Because the result is
// on a separate line, this does not consult isError — a rare failed
// write/edit is counted anyway, the same kind of small over-count the
// Claude/opencode Write paths already accept.
func parsePiEditLines(line []byte, project, tool, sessionID string, backfillCutoff time.Duration, now time.Time) []types.EditRecord {
	var raw piLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}
	if raw.Type != "message" || raw.Message == nil || raw.Message.Role != "assistant" {
		return nil
	}
	ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)
	if ts.IsZero() {
		ts = now
	}
	backfill := backfillCutoff > 0 && now.Sub(ts) > backfillCutoff
	var out []types.EditRecord
	for _, b := range raw.Message.Content {
		if b.Type != "toolCall" || b.ID == "" {
			continue
		}
		if b.Name != "write" && b.Name != "edit" {
			continue
		}
		var args piEditArgs
		if err := json.Unmarshal(b.Arguments, &args); err != nil || args.Path == "" {
			continue
		}
		var added, removed int64
		switch b.Name {
		case "write":
			added = countLines(args.Content)
		case "edit":
			for _, e := range args.Edits {
				added += countLines(e.NewText)
				removed += countLines(e.OldText)
			}
		}
		if added == 0 && removed == 0 {
			continue
		}
		out = append(out, types.EditRecord{
			EventID:      b.ID, // pi's toolCall id, provider-unique
			SessionID:    sessionID,
			Tool:         tool,
			Timestamp:    ts,
			Lang:         langFromPath(args.Path),
			LinesAdded:   added,
			LinesRemoved: removed,
			ProjectPath:  project,
			Backfill:     backfill,
		})
	}
	return out
}

// countLines returns the number of text lines in s (0 for empty). A
// trailing newline is not counted as an extra empty line — matching the
// create-content approximation used by the Claude Code / opencode parsers.
func countLines(s string) int64 {
	if s == "" {
		return 0
	}
	return int64(strings.Count(strings.TrimSuffix(s, "\n"), "\n")) + 1
}

// piSessionIDFromFilename pulls the session UUID out of a pi session file
// name, whose shape is "<ISO-timestamp>_<uuid>.jsonl" — everything after
// the last underscore. Falls back to the whole base name if there is none.
func piSessionIDFromFilename(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	if i := strings.LastIndex(base, "_"); i >= 0 {
		return base[i+1:]
	}
	return base
}
