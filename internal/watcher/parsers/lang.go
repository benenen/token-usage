package parsers

import (
	"path/filepath"
	"strings"
)

// extLang maps a lowercased file extension (without the dot) to the
// language tag stamped on EditRecords. Unknown extensions map to "other"
// — the dashboard buckets those together rather than exploding into
// one-off tags.
var extLang = map[string]string{
	"go":         "golang",
	"java":       "java",
	"rs":         "rust",
	"py":         "python",
	"js":         "javascript",
	"mjs":        "javascript",
	"cjs":        "javascript",
	"jsx":        "javascript",
	"ts":         "typescript",
	"mts":        "typescript",
	"cts":        "typescript",
	"tsx":        "typescript",
	"c":          "c",
	"h":          "c",
	"cc":         "cpp",
	"cpp":        "cpp",
	"cxx":        "cpp",
	"hpp":        "cpp",
	"hh":         "cpp",
	"cs":         "csharp",
	"rb":         "ruby",
	"php":        "php",
	"kt":         "kotlin",
	"kts":        "kotlin",
	"swift":      "swift",
	"scala":      "scala",
	"sh":         "shell",
	"bash":       "shell",
	"zsh":        "shell",
	"sql":        "sql",
	"html":       "html",
	"htm":        "html",
	"css":        "css",
	"scss":       "css",
	"less":       "css",
	"vue":        "vue",
	"svelte":     "svelte",
	"md":         "markdown",
	"markdown":   "markdown",
	"json":       "json",
	"yaml":       "yaml",
	"yml":        "yaml",
	"toml":       "toml",
	"xml":        "xml",
	"proto":      "proto",
	"lua":        "lua",
	"dart":       "dart",
	"pl":         "perl",
	"pm":         "perl",
	"r":          "r",
	"hs":         "haskell",
	"ex":         "elixir",
	"exs":        "elixir",
	"erl":        "erlang",
	"zig":        "zig",
	"groovy":     "groovy",
	"gradle":     "gradle",
	"tf":         "terraform",
	"tfvars":     "terraform",
	"hcl":        "hcl",
	"m":          "objective-c",
	"mm":         "objective-c",
	"fs":         "fsharp",
	"fsi":        "fsharp",
	"fsx":        "fsharp",
	"ml":         "ocaml",
	"mli":        "ocaml",
	"clj":        "clojure",
	"cljs":       "clojure",
	"cljc":       "clojure",
	"edn":        "clojure",
	"elm":        "elm",
	"nim":        "nim",
	"cr":         "crystal",
	"jl":         "julia",
	"f":          "fortran",
	"f90":        "fortran",
	"f95":        "fortran",
	"pas":        "pascal",
	"vb":         "vbnet",
	"s":          "assembly",
	"asm":        "assembly",
	"sol":        "solidity",
	"graphql":    "graphql",
	"gql":        "graphql",
	"nix":        "nix",
	"hx":         "haxe",
	"d":          "d",
	"sv":         "verilog",
	"vhd":        "vhdl",
	"ps1":        "powershell",
	"psm1":       "powershell",
	"bat":        "batch",
	"cmd":        "batch",
	"fish":       "shell",
	"awk":        "awk",
	"ipynb":      "notebook",
	"rst":        "restructuredtext",
	"adoc":       "asciidoc",
	"tex":        "latex",
	"org":        "org",
	"txt":        "text",
	"svg":        "svg",
	"astro":      "astro",
	"sass":       "css",
	"styl":       "css",
	"pug":        "pug",
	"hbs":        "handlebars",
	"ejs":        "javascript",
	"json5":      "json",
	"jsonc":      "json",
	"ini":        "config",
	"cfg":        "config",
	"conf":       "config",
	"env":        "config",
	"properties": "config",
	"thrift":     "thrift",
	"cmake":      "cmake",
	"bzl":        "bazel",
	"bazel":      "bazel",
	"jsonnet":    "jsonnet",
	"libsonnet":  "jsonnet",
	"prisma":     "prisma",
	"csv":        "data",
	"tsv":        "data",
}

// baseLang catches well-known extension-less (or fixed-name) files. Keys
// are matched against the lowercased basename; keep entries collision-
// safe (a generic name like "build" would misfire on ordinary files).
var baseLang = map[string]string{
	"makefile":       "makefile",
	"gnumakefile":    "makefile",
	"dockerfile":     "dockerfile",
	"containerfile":  "dockerfile",
	"cmakelists.txt": "cmake",
	"jenkinsfile":    "groovy",
	"gemfile":        "ruby",
	"rakefile":       "ruby",
	"vagrantfile":    "ruby",
	"cargo.lock":     "toml",
	"pipfile":        "toml",
	"go.mod":         "golang",
	"go.sum":         "golang",
}

// langFromPath derives the language tag for an edited file. Exact
// basenames win (CMakeLists.txt is cmake, not text), then the extension
// (case-insensitive), then "other".
func langFromPath(path string) string {
	if path == "" {
		return "other"
	}
	if lang, ok := baseLang[strings.ToLower(filepath.Base(path))]; ok {
		return lang
	}
	if ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."); ext != "" {
		if lang, ok := extLang[ext]; ok {
			return lang
		}
	}
	return "other"
}

// diffLineCounts tallies added/removed lines in unified-diff-style
// content: lines starting with '+' / '-' count, except the '+++' / '---'
// file headers. Works for Claude Code structuredPatch hunk lines,
// opencode metadata.diff, and codex apply_patch bodies alike.
func diffLineCounts(lines []string) (added, removed int64) {
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		}
	}
	return added, removed
}
