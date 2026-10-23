package parsers

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/klauspost/compress/zstd"

	"tokenusage/internal/types"
)

// deepseekHarnessParser reads DeepSeek Harness (dsh) session logs. dsh
// keeps one directory per session under
// ~/.dsh/sessions/<encoded-cwd>/session-<uuid>/ holding a single
// zstd-compressed event log, session.v4.jsonl.zstd. Byte offsets into a
// compressed stream are meaningless, so this uses codex's re-parse model:
// state is (inode, last-seen size) and any change re-reads the whole file;
// the server dedupes rows that were already ingested.
//
// The log is one event per line, each {"type", "seq", "time" (epoch ms),
// "data"}. The leading `session` event carries the session id and cwd.
// Usage rides on `assistant/message` events at data.usage, normalized by
// dsh across its provider adapters (DeepSeek's Anthropic-Messages one and
// the pi-ai one): inputTokens already excludes cache hits (cacheReadTokens)
// and outputTokens includes reasoning, so reasoningTokens is not added.
type deepseekHarnessParser struct{}

func init() { register("deepseek-harness", deepseekHarnessParser{}) }

func (deepseekHarnessParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) (ScanResult, FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ScanResult{}, FileState{}, err
	}
	inode := inodeOf(info)
	size := info.Size()
	if prev.Inode == inode && prev.Offset == size {
		return ScanResult{}, FileState{Inode: inode, Offset: size}, nil // unchanged
	}
	res, err := parseDeepseekHarnessFile(path, tool, backfillCutoff, now)
	return res, FileState{Inode: inode, Offset: size}, err
}

type dshEvent struct {
	Type string          `json:"type"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
}

type dshSession struct {
	ID  string `json:"id"`
	Cwd string `json:"cwd"`
}

type dshAssistantMessage struct {
	Message struct {
		ID     string `json:"id"`
		Source struct {
			Model string `json:"model"`
		} `json:"source"`
	} `json:"message"`
	Usage *struct {
		InputTokens      int64 `json:"inputTokens"`
		OutputTokens     int64 `json:"outputTokens"`
		CacheReadTokens  int64 `json:"cacheReadTokens"`
		CacheWriteTokens int64 `json:"cacheWriteTokens"`
	} `json:"usage"`
}

type dshToolCall struct {
	CallID    string `json:"callId"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded tool input
}

type dshToolResult struct {
	Message struct {
		ToolCallID string `json:"toolCallId"`
		IsError    bool   `json:"isError"`
	} `json:"message"`
	Error *struct{} `json:"error"` // set when the tool call failed
}

type dshEditArgs struct {
	FilePath  string `json:"file_path"`
	Content   string `json:"content"`    // write: full file body
	OldString string `json:"old_string"` // edit
	NewString string `json:"new_string"` // edit
}

// dshPendingEdit is a write/edit tool/call awaiting its tool/result. Only
// calls whose result reports success become EditRecords; a call whose
// result hasn't been written yet is picked up on a later re-parse.
type dshPendingEdit struct {
	ts             time.Time
	path           string
	added, removed int64
}

func parseDeepseekHarnessFile(path, tool string, backfillCutoff time.Duration, now time.Time) (ScanResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return ScanResult{}, err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return ScanResult{}, err
	}
	defer zr.Close()
	br := bufio.NewReaderSize(zr, 64<<10)

	var (
		res     ScanResult
		session dshSession
		pending = map[string]dshPendingEdit{}
	)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			var ev dshEvent
			if json.Unmarshal(line, &ev) == nil {
				ts := time.UnixMilli(ev.Time)
				if ev.Time == 0 {
					ts = now
				}
				backfill := backfillCutoff > 0 && now.Sub(ts) > backfillCutoff
				switch ev.Type {
				case "session":
					// The session header is the event itself, not under data.
					_ = json.Unmarshal(line, &session)
				case "assistant/message":
					var m dshAssistantMessage
					if json.Unmarshal(ev.Data, &m) != nil || m.Usage == nil || m.Message.ID == "" || m.Message.Source.Model == "" {
						break
					}
					res.Usage = append(res.Usage, types.UsageRecord{
						MessageID:           "dsh_" + session.ID + "_" + m.Message.ID,
						SessionID:           session.ID,
						Tool:                tool,
						Model:               m.Message.Source.Model,
						Timestamp:           ts,
						InputTokens:         m.Usage.InputTokens,
						OutputTokens:        m.Usage.OutputTokens,
						CacheCreationTokens: m.Usage.CacheWriteTokens,
						CacheReadTokens:     m.Usage.CacheReadTokens,
						ProjectPath:         session.Cwd,
						Backfill:            backfill,
					})
				case "tool/call":
					var c dshToolCall
					if json.Unmarshal(ev.Data, &c) != nil || c.CallID == "" || (c.Name != "write" && c.Name != "edit") {
						break
					}
					var args dshEditArgs
					if json.Unmarshal([]byte(c.Arguments), &args) != nil || args.FilePath == "" {
						break
					}
					p := dshPendingEdit{ts: ts, path: args.FilePath}
					if c.Name == "write" {
						p.added = countLines(args.Content)
					} else {
						p.added = countLines(args.NewString)
						p.removed = countLines(args.OldString)
					}
					if p.added > 0 || p.removed > 0 {
						pending[c.CallID] = p
					}
				case "tool/result":
					var r dshToolResult
					if json.Unmarshal(ev.Data, &r) != nil {
						break
					}
					p, ok := pending[r.Message.ToolCallID]
					if !ok {
						break
					}
					delete(pending, r.Message.ToolCallID)
					if r.Message.IsError || r.Error != nil {
						break
					}
					res.Edits = append(res.Edits, types.EditRecord{
						EventID:      r.Message.ToolCallID,
						SessionID:    session.ID,
						Tool:         tool,
						Timestamp:    p.ts,
						Lang:         langFromPath(p.path),
						LinesAdded:   p.added,
						LinesRemoved: p.removed,
						ProjectPath:  session.Cwd,
						Backfill:     backfillCutoff > 0 && now.Sub(p.ts) > backfillCutoff,
					})
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			// A truncated zstd frame (dsh mid-write) fails here; the
			// checkpoint isn't advanced, so the next tick retries.
			return res, rerr
		}
	}
	return res, nil
}
