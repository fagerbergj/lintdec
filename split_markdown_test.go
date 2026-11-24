package main

import (
	"slices"
	"strings"
	"testing"
)

const mdFixture = `---
title: Widget Guide
status: Draft
---
# Widget Guide

This guide explains how to install the widget.

## Install

Run the installer first. Then restart the service.

` + "```sh\nmake install\n# not a heading\n```" + `

- Download the package
- Verify the checksum

### Verify

1. Open the dashboard.
2. Check the status.

> Quoted callout text that is not the author's own prose.

<!-- comment only -->

---

## Notes

TBD

## Reference

| Name | Default |
|---|---|
| port | 8080 |
`

func unitsOf(kind string, us []Unit) []Unit {
	return slices.DeleteFunc(slices.Clone(us), func(u Unit) bool { return u.Kind != kind })
}

func span(u Unit) [2]int { return [2]int{u.Start, u.End} }

func TestSplitMarkdownBlocks(t *testing.T) {
	us := splitMarkdown("guide.md", []byte(mdFixture))
	got := map[string][][2]int{}
	for _, kind := range []string{"Paragraph", "Bullet", "Steps", "Table", "Section", "Document"} {
		for _, u := range unitsOf(kind, us) {
			got[kind] = append(got[kind], span(u))
		}
	}
	want := map[string][][2]int{
		// front matter, fenced code, the blockquote, the comment-only block and the rule are not prose
		"Paragraph": {{7, 7}, {11, 11}, {34, 34}},
		"Bullet":    {{18, 19}},
		"Steps":     {{23, 24}},
		"Table":     {{38, 40}},
		// Install and Verify end before Notes, trailing blanks trimmed; Notes (TBD) is a placeholder
		"Section":  {{9, 30}, {21, 30}, {36, 40}},
		"Document": {{1, 41}},
	}
	for kind, w := range want {
		if !slices.Equal(got[kind], w) {
			t.Errorf("%s units = %v, want %v", kind, got[kind], w)
		}
	}
}

func TestMarkdownBlockContext(t *testing.T) {
	us := splitMarkdown("guide.md", []byte(mdFixture))
	byStart := map[int]Unit{}
	for _, u := range us {
		if u.Kind == "Paragraph" || u.Kind == "Bullet" || u.Kind == "Steps" {
			byStart[u.Start] = u
		}
	}
	first, install, list, steps := byStart[7], byStart[11], byStart[18], byStart[23]
	for _, c := range []struct {
		u        Unit
		has, not []string
	}{
		{first, []string{"TITLE: Widget Guide", "STATUS: Draft", "HEADING: Widget Guide", "POSITION: first block", "NEXT LINE: ## Install"}, []string{"PREVIOUS BLOCK"}},
		{install, []string{"HEADING: Widget Guide > Install", "NEXT LINE: ```sh"}, []string{"POSITION", "PREVIOUS BLOCK"}},
		{list, []string{"HEADING: Widget Guide > Install", "PREVIOUS BLOCK:\nRun the installer first."}, nil},
		{steps, []string{"HEADING: Widget Guide > Install > Verify"}, []string{"PREVIOUS BLOCK"}},
	} {
		for _, s := range c.has {
			if !strings.Contains(c.u.Context, s) {
				t.Errorf("unit %d context lacks %q:\n%s", c.u.Start, s, c.u.Context)
			}
		}
		for _, s := range c.not {
			if strings.Contains(c.u.Context, s) {
				t.Errorf("unit %d context has %q:\n%s", c.u.Start, s, c.u.Context)
			}
		}
	}
}

func TestMarkdownWholeUnitsSkipMetadataAndCode(t *testing.T) {
	us := splitMarkdown("guide.md", []byte(mdFixture))
	doc := unitsOf("Document", us)[0]
	install := unitsOf("Section", us)[0]
	for _, u := range []Unit{doc, install} {
		if strings.Contains(u.Text, "make install") || strings.Contains(u.Text, "```") {
			t.Errorf("%s text includes fenced code:\n%s", u.Kind, u.Text)
		}
	}
	if strings.Contains(doc.Text, "title: Widget Guide") || len(strings.Split(doc.Text, "\n")) != doc.End {
		t.Errorf("document must blank front matter and keep one text line per file line")
	}
	long := strings.Repeat("Line of prose here.\n\n", mdWholeLines)
	if us := splitMarkdown("long.md", []byte("# Long\n\n## Part\n\n"+long)); len(unitsOf("Document", us))+len(unitsOf("Section", us)) != 0 {
		t.Error("units over mdWholeLines must be skipped, not sent whole")
	}
}

func TestHeadingPathsIgnoreFencesAndPopLevels(t *testing.T) {
	lines := strings.Split("# A\n## B\n```\n## not\n```\n### C\n## D\n#### E", "\n")
	heads, paths := headingPaths(lines)
	if len(heads) != 5 {
		t.Fatalf("headings = %v", heads)
	}
	for line, want := range map[int]string{1: "A", 2: "A > B", 4: "A > B", 6: "A > B > C", 7: "A > D", 8: "A > D > E"} {
		if paths[line] != want {
			t.Errorf("path at line %d = %q, want %q", line, paths[line], want)
		}
	}
}

func TestCandidateWordsAnchorToTheirOwnLine(t *testing.T) {
	u := Unit{Kind: "Paragraph", File: "a.md", Start: 1, End: 2, Text: "The cache knows nothing. The report\nsays the cache knows the port and it hides why."}
	var lines []int
	for _, c := range unitsOf("ActorVerb", candidateUnits(u, "", "", "")) {
		if strings.Contains(c.Text, "cache ⟦knows⟧ the port") {
			lines = append(lines, c.Start, c.End)
		}
	}
	if !slices.Equal(lines, []int{2, 2}) {
		t.Fatalf("repeated word anchored to %v, want line 2", lines)
	}
}

func TestListItemsAreSegmentedSeparately(t *testing.T) {
	got := mdSentences("- the daemon flushes the cache\n- it knows the reason\n- **Retry**: three times now")
	want := []string{"the daemon flushes the cache", "it knows the reason", "Retry: three times now"}
	if !slices.Equal(got, want) {
		t.Fatalf("mdSentences = %q, want %q", got, want)
	}
	s := blockSentences("A sentence that is\nhard wrapped across lines. Another one here.")
	if len(s) != 2 || s[0].lines[0] != 0 || s[0].lineAt(len(s[0].text)-1) != 1 || s[1].lines[0] != 1 {
		t.Fatalf("sentence lines: %+v", s)
	}
}

func TestBuildFindingSpanAndQuote(t *testing.T) {
	r := Rule{ID: "md-inanimate-actor", Source: "src"}
	word := buildFinding(job{unit: Unit{Kind: "ActorVerb", File: "a.md", Start: 3, End: 3, Text: "The cache ⟦knows⟧ the port."}}, r, Verdict{Confidence: 0.9}, nil)
	if word.Span != "knows" || word.Line != 3 || word.EndLine != 3 || word.Quote != "The cache ⟦knows⟧ the port." || word.Source != "src" {
		t.Fatalf("word finding: %+v", word)
	}
	long := strings.Repeat("word ", 60) + "the ⟦knows⟧ end"
	if f := buildFinding(job{unit: Unit{Kind: "ActorVerb", File: "a.md", Start: 1, End: 1, Text: long}}, r, Verdict{}, nil); !strings.HasPrefix(f.Quote, "…") || !strings.Contains(f.Quote, "⟦knows⟧") {
		t.Fatalf("long quote must keep the marked word in view: %q", f.Quote)
	}
	block := buildFinding(job{unit: Unit{Kind: "Paragraph", File: "a.md", Start: 2, End: 4, Text: "x"}}, r, Verdict{}, map[string][]string{"a.md": {"one", "  two  ", "three"}})
	if block.Span != "" || block.Quote != "two" || block.EndLine != 4 {
		t.Fatalf("block finding: %+v", block)
	}
}

func TestAbbreviationsDoNotEndSentences(t *testing.T) {
	for in, want := range map[string][]string{
		"See U.S. conventions for dates here.":             {"See U.S. conventions for dates here."},
		"The flag supports json, e.g. for pipes, and csv.": {"The flag supports json, e.g. for pipes, and csv."},
		"Add logs, metrics, etc. to the dashboard first.":  {"Add logs, metrics, etc. to the dashboard first."},
		"Ask J. Smith about the deploy window.":            {"Ask J. Smith about the deploy window."},
		"The build runs nightly. It takes an hour.":        {"The build runs nightly.", "It takes an hour."},
		"Upgrade to version 1.2. Then restart it.":         {"Upgrade to version 1.2.", "Then restart it."},
	} {
		if got := mdSentences(in); !slices.Equal(got, want) {
			t.Errorf("mdSentences(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWordyPhraseAnchorsToItsLine(t *testing.T) {
	u := Unit{Kind: "Paragraph", File: "a.md", Start: 10, End: 12, Text: "The job \"says the run is over now\" and\nretries the step until it\nis able to finish."}
	got := unitsOf("WordyPhrase", candidateUnits(u, "", "", ""))
	if len(got) != 1 || !strings.Contains(got[0].Text, "⟦is able to⟧") || got[0].Start != 12 || got[0].End != 12 {
		t.Fatalf("wordy phrase units = %+v, want one on line 12", got)
	}
}
