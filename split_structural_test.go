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
	}
	for in, want := range cases {
		if got := placeholderBody(strings.Split(in, "\n")); got != want {
			t.Errorf("placeholderBody(%q) = %v, want %v", in, got, want)
		}
	}
}
