package parsers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
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

func (opencodeParser) Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) ([]types.UsageRecord, FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, FileState{}, err
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
		return nil, FileState{Inode: inode, Offset: cursor}, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT m.id, m.session_id, m.time_created, m.time_updated, m.data,
		       COALESCE(s.directory, '')
		FROM message m
		LEFT JOIN session s ON s.id = m.session_id
		WHERE m.time_updated > ?
		ORDER BY m.time_updated, m.id`, cursor)
	if err != nil {
		return nil, FileState{Inode: inode, Offset: cursor}, err
	}
	defer rows.Close()

	var out []types.UsageRecord
	maxSeen := cursor
	for rows.Next() {
		var (
			id, sessionID, data, directory string
			timeCreated, timeUpdated       int64
		)
		if err := rows.Scan(&id, &sessionID, &timeCreated, &timeUpdated, &data, &directory); err != nil {
			return out, FileState{Inode: inode, Offset: maxSeen}, err
		}
		if timeUpdated > maxSeen {
			maxSeen = timeUpdated
		}
		rec, ok := opencodeMessageToRecord(id, sessionID, directory, tool, data,
			timeCreated, backfillCutoff, now)
		if !ok {
			continue
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return out, FileState{Inode: inode, Offset: maxSeen}, err
	}
	return out, FileState{Inode: inode, Offset: maxSeen}, nil
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
