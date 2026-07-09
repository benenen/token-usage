package parsers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO-free, matches project invariant)

	"tokenusage/internal/types"
)

// opencodeParser reads assistant-message usage rows from opencode's local
// SQLite database, currently shipped at
//   ~/.local/share/opencode/opencode.db
//
// The `message` table stores one row per turn; `message.data` is a JSON
// blob with shape:
//   {
//     "role": "assistant",
//     "modelID": "claude-sonnet-4-6",
//     "providerID": "anthropic",
//     "time": {"created": <ms>, "completed": <ms>},
//     "tokens": {"input": …, "output": …, "reasoning": …,
//                "cache": {"read": …, "write": …}},
//     "cost": <usd>,
//     "finish": "tool-calls"
//   }
// Only assistant messages have token fields; user/system rows have
// nothing to bill and are skipped. Reasoning tokens are folded into
// output to mirror the codex parser's convention.
//
// Checkpoint convention: FileState.Inode is the db's inode (lets us
// detect a wholesale rebuild) and FileState.Offset stores the highest
// time_updated (ms epoch) seen, used as the watermark for the next
// incremental scan. opencode's `time_updated` advances per row insert
// and is the natural cursor for `WHERE time_updated > ?`.
type opencodeParser struct{}

func init() { register("opencode", opencodeParser{}) }

func (opencodeParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) (ScanResult, FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ScanResult{}, FileState{}, err
	}
	inode := inodeOf(info)

	// If the db was recreated (different inode), forget the watermark
	// and re-emit everything — the server dedups on MessageID anyway.
	cursor := prev.Offset
	if prev.Inode != inode {
		cursor = 0
	}

	// Open read-only. busy_timeout protects against the brief lock
	// windows opencode itself takes during checkpoint operations.
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(2000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return ScanResult{}, FileState{Inode: inode, Offset: cursor}, err
	}
	defer db.Close()

	var res ScanResult
	// Both queries share the same cursor, and the returned watermark is
	// the max time_updated across BOTH tables — message and part rows are
	// stamped by the same local opencode process, so one watermark is
	// coherent for the pair. Slight re-reads on restart are fine (server
	// dedups on event/message id).
	maxSeen := cursor

	rows, err := db.Query(`
		SELECT m.id, m.session_id, m.time_created, m.time_updated, m.data,
		       COALESCE(s.directory, '')
		FROM message m
		LEFT JOIN session s ON s.id = m.session_id
		WHERE m.time_updated > ?
		ORDER BY m.time_updated, m.id`, cursor)
	if err != nil {
		return res, FileState{Inode: inode, Offset: cursor}, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, sessionID, data, directory string
			timeCreated, timeUpdated       int64
		)
		if err := rows.Scan(&id, &sessionID, &timeCreated, &timeUpdated, &data, &directory); err != nil {
			return res, FileState{Inode: inode, Offset: maxSeen}, err
		}
		if timeUpdated > maxSeen {
			maxSeen = timeUpdated
		}
		rec, ok := opencodeMessageToRecord(id, sessionID, directory, tool, data,
			timeCreated, backfillCutoff, now)
		if !ok {
			continue
		}
		res.Usage = append(res.Usage, rec)
	}
	if err := rows.Err(); err != nil {
		return res, FileState{Inode: inode, Offset: maxSeen}, err
	}

	edits, editMax, err := scanOpencodeParts(db, cursor, tool, backfillCutoff, now)
	if err != nil {
		return res, FileState{Inode: inode, Offset: maxSeen}, err
	}
	res.Edits = edits
	if editMax > maxSeen {
		maxSeen = editMax
	}
	return res, FileState{Inode: inode, Offset: maxSeen}, nil
}

// scanOpencodeParts reads edit/write tool-call rows from the `part`
// table (one row per tool invocation; data.state carries input +
// metadata). Databases from opencode versions that predate the part
// table are tolerated: no table -> no edits, no error.
func scanOpencodeParts(db *sql.DB, cursor int64, tool string, backfillCutoff time.Duration, now time.Time) ([]types.EditRecord, int64, error) {
	var exists int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='part'`).Scan(&exists); err != nil || exists == 0 {
		return nil, cursor, err
	}

	rows, err := db.Query(`
		SELECT p.id, p.session_id, p.time_created, p.time_updated, p.data,
		       COALESCE(s.directory, '')
		FROM part p
		LEFT JOIN session s ON s.id = p.session_id
		WHERE p.time_updated > ?
		ORDER BY p.time_updated, p.id`, cursor)
	if err != nil {
		return nil, cursor, err
	}
	defer rows.Close()

	var out []types.EditRecord
	maxSeen := cursor
	for rows.Next() {
		var (
			id, sessionID, data, directory string
			timeCreated, timeUpdated       int64
		)
		if err := rows.Scan(&id, &sessionID, &timeCreated, &timeUpdated, &data, &directory); err != nil {
			return out, maxSeen, err
		}
		if timeUpdated > maxSeen {
			maxSeen = timeUpdated
		}
		rec, ok := opencodePartToEdit(id, sessionID, directory, tool, data, timeCreated, backfillCutoff, now)
		if !ok {
			continue
		}
		out = append(out, rec)
	}
	return out, maxSeen, rows.Err()
}

// opencodePartData mirrors the part.data JSON blob for tool parts.
type opencodePartData struct {
	Type  string `json:"type"` // "tool" for tool invocations
	Tool  string `json:"tool"` // "edit", "write", "bash", …
	State *struct {
		Status string `json:"status"` // "completed" | "error" | …
		Input  struct {
			FilePath string `json:"filePath"`
			Content  string `json:"content"`
		} `json:"input"`
		Metadata struct {
			Diff string `json:"diff"` // unified diff (edit tool only)
		} `json:"metadata"`
	} `json:"state"`
}

func opencodePartToEdit(id, sessionID, directory, tool, data string,
	timeCreated int64, backfillCutoff time.Duration, now time.Time) (types.EditRecord, bool) {
	var d opencodePartData
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return types.EditRecord{}, false
	}
	if d.Type != "tool" || d.State == nil || d.State.Status != "completed" {
		return types.EditRecord{}, false
	}
	if d.State.Input.FilePath == "" {
		return types.EditRecord{}, false
	}
	var added, removed int64
	switch d.Tool {
	case "edit":
		added, removed = diffLineCounts(strings.Split(d.State.Metadata.Diff, "\n"))
	case "write":
		// Write results carry no diff — count the new content's lines as
		// added. For overwrites this over-counts slightly (old lines are
		// not visible), an accepted approximation.
		if d.State.Input.Content != "" {
			added = int64(len(strings.Split(strings.TrimSuffix(d.State.Input.Content, "\n"), "\n")))
		}
	default:
		return types.EditRecord{}, false
	}
	ts := time.UnixMilli(timeCreated)
	return types.EditRecord{
		EventID:      id, // part ids (prt_…) are globally unique
		SessionID:    sessionID,
		Tool:         tool,
		Timestamp:    ts,
		Lang:         langFromPath(d.State.Input.FilePath),
		LinesAdded:   added,
		LinesRemoved: removed,
		ProjectPath:  directory,
		Backfill:     backfillCutoff > 0 && now.Sub(ts) > backfillCutoff,
	}, true
}

type opencodeMessageData struct {
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Tokens     *struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

func opencodeMessageToRecord(id, sessionID, directory, tool, data string,
	timeCreated int64, backfillCutoff time.Duration, now time.Time) (types.UsageRecord, bool) {
	var d opencodeMessageData
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return types.UsageRecord{}, false
	}
	if d.Role != "assistant" || d.Tokens == nil {
		return types.UsageRecord{}, false
	}
	// Skip empty turns (opencode emits zero-token rows for cancellations
	// or pre-completion snapshots).
	totalOut := d.Tokens.Output + d.Tokens.Reasoning
	if d.Tokens.Input == 0 && totalOut == 0 &&
		d.Tokens.Cache.Read == 0 && d.Tokens.Cache.Write == 0 {
		return types.UsageRecord{}, false
	}
	ts := time.UnixMilli(timeCreated)
	backfill := backfillCutoff > 0 && now.Sub(ts) > backfillCutoff
	return types.UsageRecord{
		MessageID:           id, // opencode message IDs (msg_…) are already globally unique
		SessionID:           sessionID,
		Tool:                tool,
		Model:               d.ModelID,
		Timestamp:           ts,
		InputTokens:         d.Tokens.Input,
		OutputTokens:        totalOut,
		CacheCreationTokens: d.Tokens.Cache.Write,
		CacheReadTokens:     d.Tokens.Cache.Read,
		ProjectPath:         directory,
		Backfill:            backfill,
	}, true
}
