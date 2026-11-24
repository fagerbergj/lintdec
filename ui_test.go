package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestColorRespectsTerminalAndEnvironment(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	if colorEnabled(false) || !colorEnabled(true) {
		t.Fatal("color must require a terminal")
	}
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(true) {
		t.Fatal("NO_COLOR must disable color")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if colorEnabled(true) {
		t.Fatal("dumb terminals must not receive color")
	}
}

func TestProgressFitsTerminalAndShowsLatestFile(t *testing.T) {
	p := &progress{total: 10, done: 4, inflight: 2, cached: 1, last: "src/長い名前/very-long-file-name.go:23", ledgerN: 1,
		bySev: map[string]int{"error": 1}, ledger: []Finding{{Severity: "error", File: "src/長い名前/file.go", Line: 23, Rule: "wrong-assertion", Message: "An assertion does not check the result"}}}
	for _, width := range []int{30, 60, 100} {
		lines := p.lines(width, false)
		if len(lines) < 2 || !strings.Contains(strings.Join(lines, "\n"), "40%") {
			t.Fatalf("width %d: missing progress: %q", width, lines)
		}
		for _, line := range lines {
			if got := runewidth.StringWidth(line); got > width {
				t.Errorf("width %d: line is %d columns: %q", width, got, line)
			}
		}
	}
	if !strings.Contains(strings.Join(p.lines(100, false), "\n"), "very-long-file-name.go") {
		t.Fatal("progress should identify the latest file")
	}
}

func TestProgressShowsFilesWhileChecksAreRunning(t *testing.T) {
	p := &progress{total: 2, active: map[string]int{"src/second.go": 1, "src/first.go": 2}}
	got := strings.Join(p.lines(100, false), "\n")
	if !strings.Contains(got, "checking") || !strings.Contains(got, "src/first.go") || !strings.Contains(got, "src/second.go") {
		t.Fatalf("active files missing from progress: %q", got)
	}
}

func TestHumanReportContainsFindingsAndSanitizesControls(t *testing.T) {
	var out bytes.Buffer
	findings := []Finding{{Severity: "error", File: "src/a.go", Line: 12, Rule: "wrong-assertion", Confidence: .93, Message: "unexpected\x1b[2J output", Quote: "assert.True(t, ok)"}}
	renderFindings(&out, findings, false)
	got := out.String()
	for _, want := range []string{"ERROR", "src/a.go:12", "wrong-assertion", "0.93", "unexpected", "assert.True"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("untrusted output included terminal escape: %q", got)
	}
}

func TestModelPickerShowsAlignedChoicesAndActiveModel(t *testing.T) {
	var out bytes.Buffer
	choices := []modelChoice{{provider: "local", model: "small"}, {provider: "long-provider", model: "big"}}
	renderModelChoices(&out, choices, "local", "small", false)
	got := out.String()
	for _, want := range []string{"CHOOSE A MODEL", "1", "2", "local", "long-provider", "small", "big", "active"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("plain picker included terminal escapes: %q", got)
	}
}

func TestJSONReportStaysMachineReadable(t *testing.T) {
	findings := []Finding{{Severity: "warning", File: "a.go", Line: 1, Rule: "comment-long", Message: "hello"}}
	out := captureStdout(t, func() { emit(findings, "json") })
	var parsed []Finding
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || len(parsed) != 1 || parsed[0] != findings[0] {
		t.Fatalf("invalid JSON report %q: %v", out, err)
	}
}
