// Package parsers turns one tool's transcript file into UsageRecords.
//
// Each supported tool (claude-code, codex, …) lives in its own file and
// registers a concrete Parser via init(). The watcher's Scanner dispatches
// by Source.Tool: parsers.For(tool) returns the right implementation.
// To add a new tool, drop a file like parser_<tool>.go into this package
// with a struct implementing Parser and a `func init() { register(...) }`.
package parsers

import (
	"path/filepath"
	"time"

	"tokenusage/internal/types"
)

// FileState is per-file scan progress, persisted across watcher restarts.
// Inode lets us detect file rotation; Offset is how many bytes we've read
// (for tail-style parsers) OR the file's last-seen size (for re-parse
// parsers). Codex uses the latter convention; Claude Code uses the former.
type FileState struct {
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

// ScanResult is everything one Scan pass extracted from a transcript
// file: token-usage rows and file-edit events. Both come out of the same
// read (same checkpoint), so a parser never has to be scanned twice.
type ScanResult struct {
	Usage []types.UsageRecord
	Edits []types.EditRecord
}

// Parser knows how to turn one transcript file into a ScanResult. The
// `tool` argument is the user-chosen label stamped on every emitted
// record (it equals the Source.Tool from the watcher CLI; may differ
// from this parser's canonical name when a user repurposes a parser
// under a custom tag).
type Parser interface {
	Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) (ScanResult, FileState, error)
}

var registry = map[string]Parser{}

// register binds `name` to a Parser. Called from each parser file's init().
func register(name string, p Parser) { registry[name] = p }

// For returns the Parser matching `tool`, or — if none is registered —
// the claude-code parser as the historical default. Unknown tools log
// nothing here: the Scanner reports per-tick errors.
func For(tool string) Parser {
	if p, ok := registry[tool]; ok {
		return p
	}
	return registry["claude-code"]
}

// projectFromPath returns the JSONL file's parent dir name. For
// Claude Code's ~/.claude/projects/<encoded-project>/<session>.jsonl it
// gives the project tag; for Codex's daily-bucket layout it's the day
// (which is still a stable hint).
func projectFromPath(p string) string {
	return filepath.Base(filepath.Dir(p))
}
