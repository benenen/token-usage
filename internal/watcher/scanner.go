package watcher

import (
	"io/fs"
	"log"
	"path/filepath"
	"sync"
	"time"

	"tokenusage/internal/types"
)

// Source binds a JSONL root directory to a tool name. The tool name
// drives parser selection (see parser.go); the root is the directory
// the watcher walks for *.jsonl files. One watcher process can fan
// over multiple sources, each handled by its own Parser implementation.
type Source struct {
	Tool string
	Root string
}

type Scanner struct {
	Sources        []Source
	Checkpoint     *Checkpoint
	BackfillCutoff time.Duration // records older than now-cutoff are marked backfill=true
}

// Scan walks every Source root for *.jsonl in parallel goroutines (one
// per source) and returns merged records + per-file FileState. The caller
// must persist the checkpoint only AFTER successfully uploading.
//
// Per-source parallelism is a real win when one tree is on a slow disk
// or holds many small files: 600 Claude Code files + 200 Codex files
// scan side by side instead of serially.
func (s *Scanner) Scan() ([]types.UsageRecord, map[string]FileState, error) {
	type result struct {
		recs    []types.UsageRecord
		pending map[string]FileState
		err     error
	}
	results := make([]result, len(s.Sources))
	var wg sync.WaitGroup
	now := time.Now()

	for i, src := range s.Sources {
		wg.Add(1)
		go func(i int, src Source) {
			defer wg.Done()
			parser := parserFor(src.Tool)
			if parser == nil {
				results[i].err = errUnknownParser(src.Tool)
				return
			}
			pending := make(map[string]FileState)
			var recs []types.UsageRecord
			err := filepath.WalkDir(src.Root, func(path string, d fs.DirEntry, werr error) error {
				if werr != nil {
					return nil // skip unreadable subtrees
				}
				if d.IsDir() || filepath.Ext(path) != ".jsonl" {
					return nil
				}
				prev, _ := s.Checkpoint.Get(path)
				r, state, perr := parser.Scan(path, src.Tool, prev, s.BackfillCutoff, now)
				if perr != nil {
					log.Printf("scanner: parse %s (%s): %v", path, src.Tool, perr)
					return nil
				}
				if len(r) > 0 {
					recs = append(recs, r...)
				}
				pending[path] = state
				return nil
			})
			results[i] = result{recs: recs, pending: pending, err: err}
		}(i, src)
	}
	wg.Wait()

	var allRecs []types.UsageRecord
	allPending := make(map[string]FileState)
	var firstErr error
	for _, r := range results {
		if len(r.recs) > 0 {
			allRecs = append(allRecs, r.recs...)
		}
		for p, st := range r.pending {
			allPending[p] = st
		}
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}
	return allRecs, allPending, firstErr
}

// projectFromPath: parent dir name (works for both
//   ~/.claude/projects/<encoded-project>/<session>.jsonl
//   ~/.codex/sessions/YYYY/MM/DD/rollout-...jsonl   ("DD")
// — for Codex you typically don't care about the daily folder, but it's
// a stable hint about session age.
func projectFromPath(p string) string {
	return filepath.Base(filepath.Dir(p))
}

type unknownParserErr string

func (e unknownParserErr) Error() string {
	return "no parser registered for tool " + string(e)
}

func errUnknownParser(tool string) error { return unknownParserErr(tool) }
