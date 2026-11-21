package main

import (
	"bufio"
	"strings"
	"testing"
)

func TestMatchGlobBraces(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"*.{go,md}", "main.go", true},
		{"*.{go,md}", "README.md", true},
		{"*.{go,md}", "app.ts", false},
		{"pkg/**/*.{ts,tsx}", "pkg/a/b/view.tsx", true},
		{"pkg/**/*.{ts,tsx}", "pkg/a/b/view.js", false},
		{"**/fixtures/**", "a/fixtures/x.go", true},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pat, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pat, tc.path, got, tc.want)
		}
	}
}

func TestAskReturnsDefault(t *testing.T) {
	orig := stdinReader
	defer func() { stdinReader = orig }()

	stdinReader = bufio.NewReader(strings.NewReader("\ncustom\n"))
	var a, b string
	if got := ask(&a, "name", "local"); got != "local" || a != "local" {
		t.Errorf("blank input: got (%q, %q), want (local, local)", got, a)
	}
	if got := ask(&b, "name", "local"); got != "custom" || b != "custom" {
		t.Errorf("typed input: got (%q, %q), want (custom, custom)", got, b)
	}
}

func TestConnectionRejectsMalformedURL(t *testing.T) {
	err := testConnection(Provider{BaseURL: "http://bad host/v1", Model: "m"})
	if err == nil {
		t.Fatal("expected error for malformed base URL")
	}
}

func TestProgressSeverityTotals(t *testing.T) {
	p := newProgress(10, true)
	for range 10 {
		p.addFinding(Finding{Severity: "high"})
	}
	if p.ledgerN != 10 || p.bySev["high"] != 10 {
		t.Errorf("ledgerN=%d high=%d, want 10/10", p.ledgerN, p.bySev["high"])
	}
	if len(p.ledger) != 8 {
		t.Errorf("panel ring len=%d, want 8", len(p.ledger))
	}
	for range 12 {
		p.tick(0, true, "x")
	}
}
