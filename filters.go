package main

import (
	"regexp"
	"strings"
)

// Deterministic pre-filters skip model calls when a rule's structural or lexical
// prerequisite is absent, without altering unit batching or cache keys.
var (
	todoTagRe      = regexp.MustCompile(`(?i)\b(?:TODO|FIXME|HACK|XXX)(?:[\s:(\[/]|$)`)
	listConjRe     = regexp.MustCompile(`(?i)\b(?:and|or)\b`)
	readerRefRe    = regexp.MustCompile(`(?i)\b(?:you|your|you're|you've|yourself|user|users|reader|readers)\b`)
	altRefRe       = regexp.MustCompile(`(?i)\b(?:alternatives?|options?|considered|rejected|instead of|vs\.?|versus|trade-?offs?)\b`)
	adrPathTitleRe = regexp.MustCompile(`(?i)\b(?:adrs?|decisions?)\b`)
	adrHeadingRe   = regexp.MustCompile(`(?im)^#{1,3}\s+(?:status|context|decision|consequences|trade-?offs?)\b`)
	rfcPathTitleRe = regexp.MustCompile(`(?i)\b(?:rfcs?|rfds?|proposals?|design)\b`)
	rfcHeadingRe   = regexp.MustCompile(`(?im)^#{1,3}\s+(?:proposal|motivation|proposed change)\b`)
)

var ruleFilters = map[string]func(Unit) bool{
	"todo-no-owner":             func(u Unit) bool { return todoTagRe.MatchString(u.Text) },
	"comment-long":              func(u Unit) bool { return u.End-u.Start+1 >= 3 },
	"md-topic-sentence":         func(u Unit) bool { return len(blockSentences(u.Text)) >= 3 },
	"md-prose-list":             func(u Unit) bool { return hasInlineListPunct(u.Text) },
	"md-no-purpose":             func(u Unit) bool { return nonEmptyLines(u.Text) >= 3 },
	"md-blames-reader":          func(u Unit) bool { return readerRefRe.MatchString(u.Text) },
	"md-one-sided-alternatives": func(u Unit) bool { return altRefRe.MatchString(u.Text) },
	"md-adr-many-decisions":     isADRDoc,
	"md-adr-no-cost":            isADRDoc,
	"md-rfc-no-ask":             isRFCDoc,
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
		if commas >= 2 || semis >= 2 || (commas >= 1 && listConjRe.MatchString(s.text)) {
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
