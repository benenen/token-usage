package watcher

import (
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tokenusage/internal/watcher/parsers"
)

// FileState is re-exported so callers (Checkpoint persistence, the
// watcher main loop) can treat scanner state uniformly without importing
// the parsers subpackage directly.
type FileState = parsers.FileState

// Source binds a JSONL root directory to a tool name. The tool name
// drives parser selection (see internal/watcher/parsers); the root is
// the directory the watcher walks for *.jsonl. One watcher process can
// fan over multiple sources, each handled by its own Parser.
type Source struct {
	Tool string
	Root string
}

type Scanner struct {
	Sources        []Source
	Checkpoint     *Checkpoint
	BackfillCutoff time.Duration
}

// Scan walks every Source root for *.jsonl in parallel goroutines (one
// per source) and returns merged usage records + edit events + per-file
// FileState. The caller must persist the checkpoint only AFTER
// successfully uploading (or spooling) everything.
func (s *Scanner) Scan() (parsers.ScanResult, map[string]FileState, error) {
	type result struct {
		res     parsers.ScanResult
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
			parser := parsers.For(src.Tool)
			if parser == nil {
				results[i].err = errUnknownParser(src.Tool)
				return
			}
			pending := make(map[string]FileState)
			var acc parsers.ScanResult

			// A missing root must surface as a per-tick scan error — the
			// WalkDir callback below skips walk errors, so without this
			// stat a bad root logs a healthy-looking "0/0" forever.
			info, err := os.Stat(src.Root)
			if err != nil {
				results[i].err = fmt.Errorf("source root (%s): %w", src.Tool, err)
				return
			}
			// Source.Root may be a single file (e.g. opencode's SQLite db)
			// rather than a directory of *.jsonl. In that case skip WalkDir
			// and hand the parser the file path directly — checkpoint keys
			// the same way (one entry per absolute path).
			if !info.IsDir() {
				prev, _ := s.Checkpoint.Get(src.Root)
				r, state, perr := parser.Scan(src.Root, src.Tool, prev, s.BackfillCutoff, now)
				if perr != nil {
					log.Printf("scanner: parse %s (%s): %v", src.Root, src.Tool, perr)
				} else {
					acc.Usage = append(acc.Usage, r.Usage...)
					acc.Edits = append(acc.Edits, r.Edits...)
					pending[src.Root] = state
				}
				results[i] = result{res: acc, pending: pending}
				return
			}

			err = filepath.WalkDir(src.Root, func(path string, d fs.DirEntry, werr error) error {
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
				acc.Usage = append(acc.Usage, r.Usage...)
				acc.Edits = append(acc.Edits, r.Edits...)
				pending[path] = state
				return nil
			})
			results[i] = result{res: acc, pending: pending, err: err}
		}(i, src)
	}
	wg.Wait()

	var all parsers.ScanResult
	allPending := make(map[string]FileState)
	var firstErr error
	for _, r := range results {
		all.Usage = append(all.Usage, r.res.Usage...)
		all.Edits = append(all.Edits, r.res.Edits...)
		maps.Copy(allPending, r.pending)
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}
	return all, allPending, firstErr
}

type unknownParserErr string

func (e unknownParserErr) Error() string {
	return "no parser registered for tool " + string(e)
}

func errUnknownParser(tool string) error { return unknownParserErr(tool) }
