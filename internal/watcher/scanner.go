package watcher

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"tokenusage/internal/types"
)

// rawLine mirrors the Claude Code JSONL schema, picking only the fields
// we care about. Unknown fields are ignored.
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

// Source binds a JSONL root directory to a tool name. One watcher can
// fan over multiple sources; each scanned record carries its source's
// Tool tag. Today the scanner only parses Claude Code's JSONL schema;
// Codex / OpenCode parsers will land as new format branches later.
type Source struct {
	Tool string
	Root string
}

type Scanner struct {
	Sources        []Source
	Checkpoint     *Checkpoint
	BackfillCutoff time.Duration // records older than now-cutoff are marked backfill=true
}

// Scan walks every Source root for *.jsonl files and returns all new
// records since the last checkpoint, plus the new FileState per file.
// The caller must persist the checkpoint only AFTER successfully uploading.
func (s *Scanner) Scan() ([]types.UsageRecord, map[string]FileState, error) {
	pending := make(map[string]FileState)
	var out []types.UsageRecord
	var firstErr error
	for _, src := range s.Sources {
		err := filepath.WalkDir(src.Root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable subtrees
			}
			if d.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			recs, newState, scanErr := s.scanFile(path, src.Tool)
			if scanErr != nil {
				return nil
			}
			if len(recs) > 0 {
				out = append(out, recs...)
			}
			if newState != nil {
				pending[path] = *newState
			}
			return nil
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return out, pending, firstErr
}

func (s *Scanner) scanFile(path, tool string) ([]types.UsageRecord, *FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	inode := inodeOf(info)
	size := info.Size()

	prev, hadPrev := s.Checkpoint.Get(path)
	offset := prev.Offset
	switch {
	case !hadPrev:
		offset = 0
	case prev.Inode != 0 && inode != 0 && prev.Inode != inode:
		offset = 0 // file rotated/replaced
	case size < offset:
		offset = 0 // file truncated
	}
	if size == offset {
		// Nothing new, but record updated FileState so inode/offset stays fresh.
		return nil, &FileState{Inode: inode, Offset: offset}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, nil, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	cur := offset
	project := projectFromPath(path)
	var recs []types.UsageRecord
	now := time.Now()
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			cur += int64(len(line))
			if rec, ok := parseLine(line, project, tool, s.BackfillCutoff, now); ok {
				recs = append(recs, rec)
			}
		}
		if err != nil {
			// Partial trailing line (no '\n' yet): leave cur where it is so
			// we re-read this line next round.
			if errors.Is(err, io.EOF) {
				break
			}
			return recs, &FileState{Inode: inode, Offset: cur}, err
		}
	}
	return recs, &FileState{Inode: inode, Offset: cur}, nil
}

func parseLine(line []byte, project, tool string, backfillCutoff time.Duration, now time.Time) (types.UsageRecord, bool) {
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
	// Synthetic-usage rows (cache replays) can have all-zero token counts;
	// they cost nothing but still count as messages — keep them.
	ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)
	if ts.IsZero() {
		ts = now
	}
	backfill := false
	if backfillCutoff > 0 && now.Sub(ts) > backfillCutoff {
		backfill = true
	}
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

// projectFromPath: ~/.claude/projects/<encoded-project-dir>/<session>.jsonl
// returns "<encoded-project-dir>" which Claude Code derives from the original cwd.
func projectFromPath(p string) string {
	return filepath.Base(filepath.Dir(p))
}
