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
