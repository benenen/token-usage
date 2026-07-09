package types

import "time"

// UsageRecord is one assistant-message usage row, as it travels from a
// watcher to the server. Token fields are raw counts; pricing happens
// server-side.
type UsageRecord struct {
	MessageID           string    `json:"message_id"`
	RequestID           string    `json:"request_id,omitempty"`
	SessionID           string    `json:"session_id"`
	Tool                string    `json:"tool,omitempty"` // e.g. "claude-code", "codex"; defaults to "claude-code" server-side
	Model               string    `json:"model"`
	Timestamp           time.Time `json:"timestamp"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	ProjectPath         string    `json:"project_path,omitempty"`
	Backfill            bool      `json:"backfill,omitempty"`
}

// EditRecord is one file-modification event (an Edit/Write/apply_patch
// tool call that touched exactly one file), as it travels from a watcher
// to the server. Line counts come from the tool transcript's own diff;
// lang is derived watcher-side from the file extension. One multi-file
// patch (codex) fans out into one record per file.
type EditRecord struct {
	EventID      string    `json:"event_id"` // transcript-native unique id (uuid / call_id#n / part id)
	SessionID    string    `json:"session_id"`
	Tool         string    `json:"tool,omitempty"` // defaults to "claude-code" server-side
	Timestamp    time.Time `json:"timestamp"`
	Lang         string    `json:"lang"` // "golang", "java", … ; "other" when unknown
	LinesAdded   int64     `json:"lines_added"`
	LinesRemoved int64     `json:"lines_removed"`
	ProjectPath  string    `json:"project_path,omitempty"`
	Backfill     bool      `json:"backfill,omitempty"`
}

// IngestRequest carries records from a watcher. user_id is intentionally
// NOT here — it's resolved server-side from the API key in the
// Authorization header. machine_id is metadata, not identity.
//
// Records and Edits are independent streams sharing the envelope; the
// watcher sends them in separate requests so that a pre-edits server
// (whose strict decoder rejects unknown fields) still accepts the
// token-usage batches.
type IngestRequest struct {
	MachineID string        `json:"machine_id"`
	Records   []UsageRecord `json:"records,omitempty"`
	Edits     []EditRecord  `json:"edits,omitempty"`
}

type IngestResponse struct {
	Accepted        int `json:"accepted"`
	Duplicates      int `json:"duplicates"`
	EditsAccepted   int `json:"edits_accepted,omitempty"`
	EditsDuplicates int `json:"edits_duplicates,omitempty"`
}
