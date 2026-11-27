package main

import (
	"strings"
	"testing"
)

func TestStructuralCommentsAreNotLinted(t *testing.T) {
	for text, want := range map[string]bool{
		"// -----------------------------------------------":                                  true,
		"# ================================================\n# Preflight checks\n# =========": true,
		"# shellcheck source=../lib/repo-utils.sh":                                            true,
		"//nolint:errcheck":                      true,
		"//lintdec:ignore comment-tautology":     true,
		"// eslint-disable-next-line no-console": true,
		"// Read the buffer":                     false,
		"# ----\n# This section validates every flag, then exits early when the input is missing\n# ----": false,
	} {
		if got := structuralComment(Unit{Kind: "Comment", Text: text}); got != want {
			t.Errorf("structuralComment(%q) = %v, want %v", text, got, want)
		}
	}
	if structuralComment(Unit{Kind: "Function", Text: "// ----"}) {
		t.Error("only comment units are structural")
	}
}

// filterComments checks structuralComment first, then routes suppression directives to Directive.
func TestFilterCommentsReKindsSuppressions(t *testing.T) {
	in := []Unit{
		{Kind: "Comment", Text: "// ----"},
		{Kind: "Comment", Text: "//go:embed foo"},
		{Kind: "Comment", Text: "# rubocop:enable Metrics/AbcSize"},
		{Kind: "Comment", Text: "// eslint-disable-next-line no-any"},
		{Kind: "Comment", Text: "// @ts-expect-error legacy wire payload"},
		{Kind: "Comment", Text: "// NOLINTNEXTLINE(bugprone-branch-clone)"},
		{Kind: "Comment", Text: "# ruff: noqa: E501"},
		{Kind: "Comment", Text: "# flake8: noqa"},
		{Kind: "Comment", Text: "#pragma warning disable CA1303"},
		{Kind: "Comment", Text: "<!-- markdownlint-disable MD013 -->"},
		{Kind: "Comment", Text: "{/* eslint-disable-next-line react/no-danger */}"},
		{Kind: "Comment", Text: "# rubocop:disable Metrics/AbcSize"},
		{Kind: "Comment", Text: "// parseHand builds one hand from the deal. The linter flags the shadow,\n// but the second return is deliberate.\n//nolint:govet"},
		{Kind: "Comment", Text: "// Go: the GC handles cycles now"},
		{Kind: "Comment", Text: "// Lint: run make lint before pushing"},
		{Kind: "Comment", Text: "// Region: north american desks"},
		{Kind: "Comment", Text: "// Pylint settings live in pyproject"},
		{Kind: "Comment", Text: "// Type: Ignore case when matching names"},
		{Kind: "Comment", Text: "// Normal explanation comment."},
	}
	got := filterComments(in)
	if len(got) != 16 {
		t.Fatalf("want 16 surviving units (9 Directive + 7 Comment), got %d: %+v", len(got), got)
	}
	for i := range 9 {
		if got[i].Kind != "Directive" {
			t.Fatalf("got[%d] = %s (%q), want Directive", i, got[i].Kind, got[i].Text)
		}
	}
	for i := 9; i < len(got); i++ {
		if got[i].Kind != "Comment" {
			t.Fatalf("prose comment got[%d] = %s (%q), want Comment", i, got[i].Kind, got[i].Text)
		}
	}
}

func TestDirectiveCommentDoesNotMergeWithAdjacentProse(t *testing.T) {
	src := "# flake8: noqa\n# This helper explains the surrounding logic in more than five words.\ndef f():\n    return 1\n"
	units := filterComments(splitFile("sample.py", []byte(src)))
	var hasDir, hasComment bool
	for _, u := range units {
		if u.Kind == "Directive" && strings.Contains(u.Text, "flake8: noqa") {
			hasDir = true
		}
		if u.Kind == "Comment" && strings.Contains(u.Text, "explains the surrounding logic") {
			hasComment = true
		}
	}
	if !hasDir || !hasComment {
		t.Fatalf("expected separate Directive and Comment units, got: %+v", units)
	}
}

func TestDirectiveIncludesPrecedingRationaleComment(t *testing.T) {
	src := "#!/usr/bin/env bash\n# Splitting $EXTRA into separate CLI flags is intentional.\n# shellcheck disable=SC2206\n[ -n \"${EXTRA:-}\" ] && ARGS+=($EXTRA)\n"
	units := filterComments(splitFile("run.sh", []byte(src)))
	var dir Unit
	var hasComment bool
	for _, u := range units {
		if u.Kind == "Directive" {
			dir = u
		}
		if u.Kind == "Comment" && strings.Contains(u.Text, "Splitting $EXTRA") {
			hasComment = true
		}
	}
	if dir.Kind != "Directive" || dir.Start != 3 || !strings.Contains(dir.Text, "Splitting $EXTRA") || !strings.Contains(dir.Text, "SC2206") {
		t.Fatalf("expected Directive anchored at line 3 with preceding rationale, got: %+v", dir)
	}
	if !hasComment {
		t.Fatalf("expected preceding prose comment to remain lintable as Comment: %+v", units)
	}
}

func TestPrecedingRationaleStopConditions(t *testing.T) {
	cases := []struct {
		name string
		u    Unit
		want string
	}{
		{"blank line breaks", Unit{Start: 4, Context: "2\t# rationale\n5\tcmd"}, ""},
		{"code line breaks", Unit{Start: 4, Context: "2\t# rationale\n3\tx=1\n5\tcmd"}, ""},
		{"tool directive breaks", Unit{Start: 4, Context: "2\t# rationale\n3\t# shellcheck source=a.sh\n5\tcmd"}, ""},
		{"outside 8-line window", Unit{Start: 12, Context: "2\t# stale rationale\n13\tcmd"}, ""},
	}
	for _, c := range cases {
		if got := precedingRationale(c.u); got != c.want {
			t.Errorf("%s: precedingRationale = %q, want %q", c.name, got, c.want)
		}
	}
	// In Go, adjacent comment lines merge into one Comment group (stays Comment when >5 words);
	// a blank-separated directive stays Directive and keeps +-8 lines in Context via goCommentContext.
	goSrc := "package p\n\n// Long prose rationale explaining why shadowing is deliberate here.\n//nolint:govet\nfunc a() {}\n\n// Reason in separate group.\n\n//nolint:errcheck\nfunc b() {}\n"
	units := filterComments(splitFile("sample.go", []byte(goSrc)))
	var sawMergedComment, sawDirWithCtx bool
	for _, u := range units {
		if u.Kind == "Comment" && strings.Contains(u.Text, "nolint:govet") {
			sawMergedComment = true
		}
		if u.Kind == "Directive" && strings.Contains(u.Text, "nolint:errcheck") && strings.Contains(u.Context, "Reason in separate group.") {
			sawDirWithCtx = true
		}
	}
	if !sawMergedComment || !sawDirWithCtx {
		t.Fatalf("expected merged Go Comment and blank-separated Directive with Context, got: %+v", units)
	}
}

func TestSplitFileLintConfig(t *testing.T) {
	for path, want := range map[string]bool{
		"eslint.config.mjs":              true,
		"sub/.eslintrc.json":             true,
		"biome.json":                     true,
		"ruff.toml":                      true,
		"pyproject.toml":                 true,
		".golangci.yml":                  true,
		".golangci.toml":                 true,
		".rubocop.yml":                   true,
		"lintdec.yaml":                   true,
		".github/workflows/lintdec.yaml": false,
		"main.go":                        false,
	} {
		if got := isLintConfig(path); got != want {
			t.Errorf("isLintConfig(%q) = %v, want %v", path, got, want)
		}
	}
	yamlUnits := splitFile("lintdec.yaml", []byte("off:\n  - swallowed-error\n"))
	if len(yamlUnits) != 1 || yamlUnits[0].Kind != "LintConfig" || yamlUnits[0].Start != 1 || yamlUnits[0].End != 2 {
		t.Fatalf("expected single LintConfig unit for yaml, got %+v", yamlUnits)
	}
	jsUnits := splitFile("eslint.config.mjs", []byte("// TODO fix\nexport default [{ rules: { 'no-unused-vars': 'off' } }];\n"))
	var hasLC, hasComment bool
	for _, u := range jsUnits {
		if u.Kind == "LintConfig" {
			hasLC = true
		}
		if u.Kind == "Comment" {
			hasComment = true
		}
	}
	if !hasLC || !hasComment {
		t.Fatalf("JS lint config must emit LintConfig plus tree-sitter units, got %+v", jsUnits)
	}
	if empty := splitFile("lintdec.yaml", []byte("   \n")); empty != nil {
		t.Fatalf("expected nil for blank lint config, got %+v", empty)
	}
	pyPkgOnly := "[project]\nname = \"demo\"\n"
	if got := splitFile("pyproject.toml", []byte(pyPkgOnly)); got != nil {
		t.Fatalf("packaging-only pyproject.toml should emit no LintConfig, got %+v", got)
	}
	pyWithRuff := "[project]\nname = \"demo\"\n\n[tool.ruff.lint]\nignore = [\"F841\"]\n\n[tool.hatch.build]\npackages = [\"demo\"]\n\n[tool.mypy]\nstrict = false\n"
	pyUnits := splitFile("pyproject.toml", []byte(pyWithRuff))
	if len(pyUnits) != 1 || pyUnits[0].Kind != "LintConfig" || pyUnits[0].Start != 4 || pyUnits[0].End != 11 {
		t.Fatalf("pyproject.toml LintConfig unexpected: %+v", pyUnits)
	}
	if !strings.Contains(pyUnits[0].Text, "F841") || !strings.Contains(pyUnits[0].Text, "strict = false") || strings.Contains(pyUnits[0].Text, "hatch") {
		t.Fatalf("pyproject.toml LintConfig should keep only linter sections: %q", pyUnits[0].Text)
	}
}

func TestSplitFileSkipsGeneratedCode(t *testing.T) {
	genTS := "/* eslint-disable */\n// This file was automatically generated by TanStack Router.\nexport const x = 1;\n"
	if got := splitFile("routeTree.gen.ts", []byte(genTS)); got != nil {
		t.Fatalf("generated TS file should produce no units, got %+v", got)
	}
	genGo := "// Code generated by protoc-gen-go. DO NOT EDIT.\npackage pb\nfunc Foo() {}\n"
	if got := splitFile("foo.pb.go", []byte(genGo)); got != nil {
		t.Fatalf("generated Go file should produce no units, got %+v", got)
	}
	normalGo := "package p\n// Helper comment.\nfunc Foo() {}\n"
	if got := splitFile("foo.go", []byte(normalGo)); len(got) == 0 {
		t.Fatal("handwritten Go file was wrongly skipped")
	}
	mdWithBanner := "<!-- DO NOT EDIT manually -->\n# Guide\n\nHello.\n"
	if got := splitFile("README.md", []byte(mdWithBanner)); len(got) == 0 {
		t.Fatal("markdown file should not be skipped by code-gen header check")
	}
}

func TestPlaceholderBody(t *testing.T) {
	cases := map[string]bool{
		"<!-- list channels -->":                   true,
		"| Team | Contact |\n|---|---|\n|  |  |":   true,
		"| Team | Contact |\n|---|---|\n| A | b |": false,
		"Real text.":             false,
		"### Sub\n<!-- todo -->": true,
		"[sh code block]":        true,
		"[code block]":           true,
		"[See the code block]":   false,
	}
	for in, want := range cases {
		if got := placeholderBody(strings.Split(in, "\n")); got != want {
			t.Errorf("placeholderBody(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDigestForNonGoKeywords(t *testing.T) {
	lines := []string{
		"function check(items) {",
		"  const a = 1;",
		"  const b = 2;",
		"  const noise = 3;",
		"  while (items.length > 0) {",
		"    try {",
		"      expect(items[0]).toBe(1);",
		"    } catch (err) {",
		"      throw err;",
		"    }",
		"  }",
		"  except ValueError:",
		"    raise RuntimeError()",
		"}",
	}
	got := digestFor(Unit{Start: 1, End: len(lines)}, lines)
	for _, want := range []string{"while (", "try {", "expect(", "catch (", "throw err", "except ValueError:", "raise RuntimeError()"} {
		if !strings.Contains(got, want) {
			t.Errorf("digestFor missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "const noise") {
		t.Errorf("digestFor should omit non-branch body lines:\n%s", got)
	}
}
