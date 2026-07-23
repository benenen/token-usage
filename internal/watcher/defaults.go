package watcher

import (
	"os"
	"path/filepath"
	"runtime"
)

// KnownToolDefaults maps each built-in tool to its canonical on-disk root,
// relative to a user's home. AutoDetectSources walks this list against
// every home dir on the box and returns the subset whose root actually
// exists.
//
// To register a new auto-detectable tool, add an entry here AND register
// its Parser via RegisterParser in parser_<tool>.go.
var KnownToolDefaults = []struct {
	Tool    string
	Subpath string
}{
	{Tool: "claude-code", Subpath: ".claude/projects"},
	{Tool: "codex", Subpath: ".codex/sessions"},
	{Tool: "opencode", Subpath: ".local/share/opencode/opencode.db"},
	{Tool: "pi", Subpath: ".pi/agent/sessions"},
}

// AutoDetectSources returns every KnownToolDefault root that exists across
// every user home on the box — the supplied `home`, plus everyone under
// the platform's user-profile base (C:\Users on Windows, /Users on macOS,
// /home elsewhere). This matters in two practical cases: a multi-user box,
// and a Windows service running as LocalSystem (whose `home` is the empty
// systemprofile dir and would otherwise scan nothing). Each match becomes
// a Source carrying the tool tag and the absolute root path; duplicates
// (when `home` is also under the base) are deduplicated by (Tool, Root).
//
// The root may be either a directory (claude-code, codex) or a regular
// file (opencode's SQLite db); Scanner handles both.
func AutoDetectSources(home string) []Source {
	var out []Source
	seen := make(map[string]bool)
	for _, h := range candidateHomes(home) {
		for _, ds := range KnownToolDefaults {
			p := filepath.Join(h, ds.Subpath)
			info, err := os.Stat(p)
			if err != nil || !(info.IsDir() || info.Mode().IsRegular()) {
				continue
			}
			key := ds.Tool + "\x00" + p
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Source{Tool: ds.Tool, Root: p})
		}
	}
	return out
}

// candidateHomes returns the list of home directories to scan: always the
// supplied `home`, plus every interactive-looking entry under the
// platform's profile base. Built-in pseudo-profiles (Public, Default,
// Shared, etc.) are skipped.
func candidateHomes(home string) []string {
	homes := []string{home}
	base := profileBase()
	entries, err := os.ReadDir(base)
	if err != nil {
		return homes
	}
	for _, e := range entries {
		if !e.IsDir() || skipProfile[e.Name()] {
			continue
		}
		homes = append(homes, filepath.Join(base, e.Name()))
	}
	return homes
}

func profileBase() string {
	switch runtime.GOOS {
	case "windows":
		return `C:\Users`
	case "darwin":
		return "/Users"
	default:
		return "/home"
	}
}

// skipProfile lists directory names found under the profile base that
// aren't real user homes — Windows ships several built-in pseudo-profiles
// and macOS has /Users/Shared. Linux's /home is typically clean, but we
// keep the same map to avoid platform-specific branching.
var skipProfile = map[string]bool{
	"Default":            true,
	"Default User":       true,
	"Public":             true,
	"All Users":          true,
	"WDAGUtilityAccount": true,
	"Shared":             true,
}
