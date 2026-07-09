package parsers

import "testing"

func TestLangFromPath(t *testing.T) {
	cases := []struct {
		path, want string
	}{
		{"/w/proj/main.go", "golang"},
		{"/w/proj/src/Main.java", "java"},
		{"/w/proj/lib.rs", "rust"},
		{"/w/proj/app.PY", "python"}, // extension match is case-insensitive
		{"/w/proj/comp.tsx", "typescript"},
		{"/w/proj/comp.jsx", "javascript"},
		{"/w/proj/mod.mjs", "javascript"},
		{"/w/proj/query.sql", "sql"},
		{"/w/proj/run.sh", "shell"},
		{"/w/proj/style.scss", "css"},
		{"/w/proj/pom.xml", "xml"},
		{"/w/proj/mapper.XML", "xml"},
		{"/w/proj/README.md", "markdown"},
		{"/w/proj/Makefile", "makefile"},
		{"/w/proj/Dockerfile", "dockerfile"},
		{"/w/proj/noext", "other"},
		{"/w/proj/data.xyzunknown", "other"},
		{"", "other"},
		// extended coverage
		{"/w/proj/App.m", "objective-c"},
		{"/w/proj/core.clj", "clojure"},
		{"/w/proj/sim.jl", "julia"},
		{"/w/proj/deploy.ps1", "powershell"},
		{"/w/proj/analysis.ipynb", "notebook"},
		{"/w/proj/schema.graphql", "graphql"},
		{"/w/proj/flake.nix", "nix"},
		{"/w/proj/token.sol", "solidity"},
		{"/w/proj/paper.tex", "latex"},
		{"/w/proj/icon.svg", "svg"},
		{"/w/proj/vars.tfvars", "terraform"},
		{"/w/proj/rules.bzl", "bazel"},
		{"/w/proj/Jenkinsfile", "groovy"},
		{"/w/proj/Gemfile", "ruby"},
		{"/w/proj/CMakeLists.txt", "cmake"},
		{"/w/proj/go.mod", "golang"},
		{"/w/proj/comp.astro", "astro"},
		{"/w/proj/main.f90", "fortran"},
		{"/w/proj/prog.asm", "assembly"},
		{"/w/proj/query.prisma", "prisma"},
	}
	for _, c := range cases {
		if got := langFromPath(c.path); got != c.want {
			t.Errorf("langFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestDiffLineCounts(t *testing.T) {
	lines := []string{
		"Index: /w/proj/main.go",
		"===================================================================",
		"--- /w/proj/main.go",
		"+++ /w/proj/main.go",
		"@@ -1,3 +1,4 @@",
		" context line",
		"+added one",
		"+added two",
		"-removed one",
		"", // blank context line
	}
	added, removed := diffLineCounts(lines)
	if added != 2 || removed != 1 {
		t.Errorf("diffLineCounts = (+%d, -%d), want (+2, -1)", added, removed)
	}
}
