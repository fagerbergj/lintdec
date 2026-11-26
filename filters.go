package main

import (
	"regexp"
	"strings"
)

// Deterministic pre-filters skip model calls when a rule's trigger is absent;
// each proxy is chosen to preserve that trigger without altering batching or cache keys.
var (
	todoTagRe         = regexp.MustCompile(`(?i)(?:^|[^-\w])(?:TODO|FIXME|HACK|XXX)(?:[^-\w]|$)`)
	listConjRe        = regexp.MustCompile(`(?i)\b(?:and|or)\b`)
	readerRefRe       = regexp.MustCompile(`(?i)\b(?:you|your|you're|you've|yourself|user|users|reader|readers)\b`)
	altRefRe          = regexp.MustCompile(`(?i)\b(?:alternatives?|options?|considered|rejected|instead of|vs\.?|versus|trade-?offs?)\b`)
	adrPathTitleRe    = regexp.MustCompile(`(?i)\b(?:adrs?|decisions?)\b`)
	adrHeadingRe      = regexp.MustCompile(`(?im)^#{1,3}\s+(?:status|context|decision|consequences|trade-?offs?)\b`)
	rfcPathTitleRe    = regexp.MustCompile(`(?i)\b(?:rfcs?|rfds?|proposals?|design)\b`)
	rfcHeadingRe      = regexp.MustCompile(`(?im)^#{1,3}\s+(?:proposal|motivation|proposed change|detailed design|design)\b`)
	testSkipRe        = regexp.MustCompile(`(?i)(?:^|[^-\w])(?:t\.Skip\w*|test\.skip|it\.skip|describe\.skip|xit|xtest|@Disabled|@Ignore|@pytest\.mark\.skip|unittest\.skip)(?:[^-\w]|$)`)
	testMockRe        = regexp.MustCompile(`(?i)\b(?:vi\.fn|vi\.mock|vi\.spyOn|jest\.fn|jest\.mock|jest\.spyOn|MagicMock|create_autospec|sinon\.|gomock|mockk|Mockito|unittest\.mock)\b|\bpatch\s*\(`)
	testSharedStateRe = regexp.MustCompile(`(?i)(?:os\.(?:Set|Unset)env|process\.env\.\w+\s*=\s*[^=\s]|process\.env\[[^\]]+\]\s*=\s*[^=\s]|delete\s+process\.env|os\.environ\[|monkeypatch\.setenv|os\.Chdir|process\.chdir|(?:/tmp/|localhost:\d|127\.0\.0\.1:\d))`)
	remediationHeadRe = regexp.MustCompile(`(?i)^#{1,6}\s+.*(?:troubleshoot|error|warning|failure|debug|known issues|faq|caveat)`)
)

// ruleFilters maps rule IDs to deterministic pre-filters; suppression-no-reason filters by construction via Directive kind.
var ruleFilters = map[string]func(Unit) bool{
	"todo-no-owner":             func(u Unit) bool { return todoTagRe.MatchString(u.Text) },
	"comment-long":              func(u Unit) bool { return u.End-u.Start+1 >= 3 || len(strings.Fields(u.Text)) >= 50 },
	"md-topic-sentence":         func(u Unit) bool { return len(blockSentences(u.Text)) >= 3 },
	"md-prose-list":             func(u Unit) bool { return hasInlineListPunct(u.Text) },
	"md-no-purpose":             func(u Unit) bool { return nonEmptyLines(u.Text) >= 3 },
	"md-blames-reader":          func(u Unit) bool { return readerRefRe.MatchString(u.Text) },
	"md-one-sided-alternatives": func(u Unit) bool { return altRefRe.MatchString(u.Text) },
	"md-adr-many-decisions":     isADRDoc,
	"md-adr-no-cost":            isADRDoc,
	"md-rfc-no-ask":             isRFCDoc,
	"test-ignored-no-reason":    func(u Unit) bool { return testSkipRe.MatchString(u.Text) },
	"test-over-mocked":          func(u Unit) bool { return testMockRe.MatchString(u.Text) },
	"test-shared-state":         func(u Unit) bool { return testSharedStateRe.MatchString(u.Text) },
	"md-no-remediation":         func(u Unit) bool { return remediationHeadRe.MatchString(firstNonEmptyLine(u.Text)) },
}

func ruleEligible(r Rule, u Unit) bool {
	if fn, ok := ruleFilters[r.ID]; ok {
		return fn(u)
	}
	return true
}

func hasInlineListPunct(text string) bool {
	for _, s := range blockSentences(text) {
		commas := strings.Count(s.text, ",")
		semis := strings.Count(s.text, ";")
		conj := len(listConjRe.FindAllStringIndex(s.text, 2))
		if commas >= 2 || semis >= 2 || conj >= 2 || (commas >= 1 && conj >= 1) {
			return true
		}
	}
	return false
}

func nonEmptyLines(text string) int {
	n := 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func firstHeadingLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "# ") {
			return t
		}
	}
	return ""
}

func isADRDoc(u Unit) bool {
	if adrPathTitleRe.MatchString(u.File) || adrPathTitleRe.MatchString(firstHeadingLine(u.Text)) {
		return true
	}
	return len(adrHeadingRe.FindAllStringIndex(u.Text, 2)) >= 2
}

func isRFCDoc(u Unit) bool {
	if rfcPathTitleRe.MatchString(u.File) || rfcPathTitleRe.MatchString(firstHeadingLine(u.Text)) {
		return true
	}
	return rfcHeadingRe.MatchString(u.Text)
}

func firstNonEmptyLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
