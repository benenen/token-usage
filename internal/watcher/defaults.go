package watcher

import (
	"os"
	"path/filepath"
)

// KnownToolDefaults maps each built-in tool to its canonical on-disk root,
// relative to the user's home. AutoDetectSources walks this list and
// returns the subset whose root actually exists on the current machine.
//
// To register a new auto-detectable tool, add an entry here AND register
// its Parser via RegisterParser in parser_<tool>.go.
var KnownToolDefaults = []struct {
	Tool    string
	Subpath string
}{
	{Tool: "claude-code", Subpath: ".claude/projects"},
	{Tool: "codex", Subpath: ".codex/sessions"},
}

// AutoDetectSources returns the default sources whose root directory
// actually exists under `home`. Useful when the user doesn't pass any
// --source flag — the watcher picks up every tool installed on the box.
func AutoDetectSources(home string) []Source {
	var out []Source
	for _, ds := range KnownToolDefaults {
		p := filepath.Join(home, ds.Subpath)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			out = append(out, Source{Tool: ds.Tool, Root: p})
		}
	}
	return out
}
