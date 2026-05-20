package watcher

import (
	"time"

	"tokenusage/internal/types"
)

// Parser turns one tool's transcript file into UsageRecords. Each tool
// (claude-code, codex, …) registers a concrete Parser; the scanner
// dispatches by Source.Tool. Implementations decide whether they want
// to tail incrementally (Claude Code keeps a byte offset) or re-parse
// the whole file each time (Codex's sessions are small and the inline
// parser keeps cross-line state).
//
// `tool` is the user-chosen label stamped onto every emitted record;
// it equals the Source.Tool from the CLI, which may or may not match
// the parser's "canonical" name (it's fine for a custom label to use
// the same parsing strategy as claude-code, etc).
type Parser interface {
	Scan(path, tool string, prev FileState, backfillCutoff time.Duration, now time.Time) ([]types.UsageRecord, FileState, error)
}

var parsers = map[string]Parser{}

// RegisterParser is called from each parser's init(). Last-one-wins on
// duplicate names, which lets tests or downstream code swap behavior.
func RegisterParser(name string, p Parser) { parsers[name] = p }

// parserFor returns the parser matching tool, or — if none matches —
// the claude-code parser as the historical default. Unknown tools log
// nothing here: the watcher main loop reports per-tick errors.
func parserFor(tool string) Parser {
	if p, ok := parsers[tool]; ok {
		return p
	}
	return parsers["claude-code"]
}
