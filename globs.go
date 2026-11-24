package main

import (
	"path"
	"path/filepath"
	"strings"
)

// matchGlob matches a slash-separated relative path against a pattern: '**' spans path
// segments, '*' and '?' match within one, top-level '{a,b}' alternates.
func matchGlob(pattern, p string) bool {
	patterns := []string{pattern}
	if before, after, ok := strings.Cut(pattern, "{"); ok {
		if end := strings.Index(after, "}"); end >= 0 {
			rest := after[end+1:]
			var out []string
			for _, alt := range strings.Split(after[:end], ",") {
				out = append(out, before+alt+rest)
			}
			patterns = out
		}
	}
	for _, p2 := range patterns {
		if segMatch(strings.Split(p2, "/"), strings.Split(p, "/")) {
			return true
		}
	}
	return false
}

func segMatch(pat, pathSegs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(pathSegs); i++ {
				if segMatch(pat[1:], pathSegs[i:]) {
					return true
				}
			}
			return false
		}
		if len(pathSegs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], pathSegs[0])
		if err != nil || !ok {
			return false
		}
		pat, pathSegs = pat[1:], pathSegs[1:]
	}
	return len(pathSegs) == 0
}

// matchAnyGlob matches p against any pattern, trying both the full relative path and the
// basename, so "*.go" and "**/vendor/**" both feel natural.
func matchAnyGlob(patterns []string, p string) bool {
	base := filepath.Base(p)
	for _, g := range patterns {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if matchGlob(g, p) || matchGlob(g, base) {
			return true
		}
	}
	return false
}
