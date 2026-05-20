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

// IngestRequest carries records from a watcher. user_id is intentionally
// NOT here — it's resolved server-side from the API key in the
// Authorization header. machine_id is metadata, not identity.
type IngestRequest struct {
	MachineID string        `json:"machine_id"`
	Records   []UsageRecord `json:"records"`
}

type IngestResponse struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
}
