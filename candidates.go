package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/jdkato/prose/v2"
)

// Candidate units: code proposes one word in one sentence and Jev judges only that
// word, the split Clark, Martin & Mahowald (arXiv:2610.29162) used with Jev.

// spanKinds are units whose Text is one sentence with the judged word marked ⟦like this⟧.
var spanKinds = map[string]bool{"ActorVerb": true, "NounVerb": true, "WordyPhrase": true}

// tagModel is loaded once; nil (tagging off) if the embedded model fails to load.
var tagModel = sync.OnceValue(func() *prose.Model {
	d, err := prose.NewDocument("warm up", prose.WithExtraction(false), prose.WithSegmentation(false))
	if err != nil {
		return nil
	}
	return d.Model
})

type tagged struct{ text, tag string }

// tagSentence: prose's sentence segmenter costs ~70ms a call, so sentences are split by mdSentences.
func tagSentence(s string) []tagged {
	if tagModel() == nil {
		return nil
	}
	d, err := prose.NewDocument(s, prose.WithExtraction(false), prose.WithSegmentation(false), prose.UsingModel(tagModel()))
	if err != nil {
		return nil
	}
	out := make([]tagged, 0, len(d.Tokens()))
	for _, t := range d.Tokens() {
		out = append(out, tagged{t.Text, t.Tag})
	}
	return out
}

var (
	mdLink     = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdListMark = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	wordEnd    = regexp.MustCompile(`[.!?]["')\]]*$`)
	mdMarkup   = strings.NewReplacer("**", "", "__", "", "`", "", "|", " ")
)

// sentence is one prose sentence with markup removed; words[k] is the byte offset of its k-th
// word in text and lines[k] the 0-based block line that word came from.
type sentence struct {
	text         string
	words, lines []int
}

// lineAt: the block line of the word containing byte offset off.
func (s sentence) lineAt(off int) int {
	k, _ := slices.BinarySearch(s.words, off+1)
	return s.lines[max(0, k-1)]
}

// blockSentences splits a Markdown block into sentences, each list item on its own so one
// item's grammar never runs into the next.
func blockSentences(block string) []sentence {
	block = replaceKeepingLines(htmlComment, block, func(string) string { return "" })
	block = replaceKeepingLines(mdLink, block, func(m string) string { return mdLink.ReplaceAllString(m, "$1") })
	ws := blockWords(block)
	var out []sentence
	var cur sentence
	flush := func() {
		if len(cur.words) >= 3 {
			out = append(out, cur)
		}
		cur = sentence{}
	}
	for k, w := range ws {
		if w.item {
			flush()
		}
		if cur.text != "" {
			cur.text += " "
		}
		cur.words, cur.lines = append(cur.words, len(cur.text)), append(cur.lines, w.line)
		cur.text += w.text
		if endsSentence(ws, k) {
			flush()
		}
	}
	flush()
	return out
}

// blockWord is one word of a block with markup removed; item marks the first word of a list item.
type blockWord struct {
	text string
	line int
	item bool
}

func blockWords(block string) []blockWord {
	var ws []blockWord
	for ln, l := range strings.Split(block, "\n") {
		item := mdListMark.MatchString(l)
		for _, w := range strings.Fields(mdMarkup.Replace(mdListMark.ReplaceAllString(l, ""))) {
			ws = append(ws, blockWord{w, ln, item})
			item = false
		}
	}
	return ws
}

// abbreviation: dotted letters ("e.g.", "U.S."), an initial ("J."), or a common short form ("Dr.", "vs."),
// none of which ends a sentence; fusing two real sentences is safer than cutting one in half.
var abbreviation = regexp.MustCompile(`^(?:[A-Za-z]{1,3}\.){2,}$|^[A-Z]\.$|^(?i:etc|vs|cf|approx|dr|mr|mrs|ms|jr|sr|inc|ltd|st|fig|no)\.$`)

// endsSentence: word k ends in . ! or ? and the next word, if any, could start a sentence;
// "etc. for pipes" continues because a lowercase word is no sentence start.
func endsSentence(ws []blockWord, k int) bool {
	w := strings.TrimRight(ws[k].text, `"')]`)
	if !wordEnd.MatchString(ws[k].text) || abbreviation.MatchString(w) {
		return false
	}
	if k+1 == len(ws) || ws[k+1].item {
		return true
	}
	next := ws[k+1].text
	i := strings.IndexFunc(next, unicode.IsLetter)
	return i < 0 || !unicode.IsLower([]rune(next[i:])[0])
}

// replaceKeepingLines replaces each match but keeps its line breaks, so later lines keep their numbers.
func replaceKeepingLines(re *regexp.Regexp, s string, repl func(string) string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		r := repl(m)
		return r + strings.Repeat("\n", strings.Count(m, "\n")-strings.Count(r, "\n"))
	})
}

// mdSentences: the prose sentences of a Markdown block, with markup removed.
func mdSentences(block string) []string {
	var out []string
	for _, s := range blockSentences(block) {
		out = append(out, s.text)
	}
	return out
}

// mark wraps the i-th token of s in ⟦⟧ and returns its byte offset, finding tokens in order so
// repeats resolve correctly.
func mark(s string, toks []tagged, i int) (string, int) {
	cur := 0
	for j, t := range toks {
		k := strings.Index(s[cur:], t.text)
		if k < 0 {
			return "", 0
		}
		k += cur
		if j == i {
			return s[:k] + "⟦" + t.text + "⟧" + s[k+len(t.text):], k
		}
		cur = k + len(t.text)
	}
	return "", 0
}

// mindVerbs need a mind: a thing that knows or wants is personified (both judges agreed on every
// labeled case), while refuse, decide, and feel have literal technical/UX senses.
var mindVerbs = toSet("know knows knew want wants wanted think thinks thought believe believes believed " +
	"understand understands understood wish wishes hope hopes like likes love loves hate hates " +
	"care cares fear fears remember remembers forget forgets forgot learn learns learned lie lies lied " +
	"pretend pretends worry worries trust trusts realize realizes realized notice notices prefer prefers " +
	"enjoy enjoys complain complains insist insists agree agrees disagree disagrees")

// plainVerbs are common verbs that only waste a Jev call as noun-as-verb candidates.
var plainVerbs = toSet("is are was were be been has have had does do did can could will would should may might must " +
	"use run call make take give get set add keep see read write send open close move check list name record " +
	"delete create update test edit change mention trigger grant pilot merge commit deploy push pull cannot")

var personPronouns = toSet("i you he she we they who someone everyone anyone people " +
	"user users engineer engineers developer developers team teams owner owners reviewer reviewers admin admins " +
	"customer customers author authors reader readers operator operators member members maintainer maintainers " +
	"agent agents llm llms model models assistant assistants")

var wordToken = regexp.MustCompile(`^[A-Za-z][a-z]+$|^[A-Z]{2,}s?$`)

func toSet(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

// actorVerb: a finite verb whose subject, just before it, is a thing rather than a person.
func actorVerb(toks []tagged, i int) bool {
	t := toks[i]
	if !strings.HasPrefix(t.tag, "VB") || t.tag == "VBN" || t.tag == "VBG" || !mindVerbs[strings.ToLower(t.text)] {
		return false // passive "is still wanted" has no actor to personify
	}
	j := i - 1
	for j >= 0 && (toks[j].tag == "RB" || toks[j].tag == "MD") {
		j--
	}
	return j >= 0 && thingSubject(toks[j])
}

var thingDeterminers = toSet("this that which")

// thingSubject: a noun, closing parenthesis, "it", or "this/that/which", but never a person pronoun.
func thingSubject(s tagged) bool {
	if personPronouns[strings.ToLower(strings.SplitN(s.text, "'", 2)[0])] { // "we'd" is still we
		return false
	}
	return strings.HasPrefix(s.tag, "NN") || s.text == ")" || (s.tag == "PRP" && strings.EqualFold(s.text, "it")) ||
		((s.tag == "DT" || s.tag == "WDT") && thingDeterminers[strings.ToLower(s.text)])
}

var objectWords = toSet("it them this that these those everything me us")
var verbLead = toSet(", ; : and then to just please can should will must so or")

// nounVerb: a word in verb position (before an object pronoun, after a sentence start or
// modal) that is a capitalized name, an acronym, or a word the tagger reads as a noun.
func nounVerb(toks []tagged, i int) bool {
	t := toks[i]
	w := strings.ToLower(t.text)
	if plainVerbs[w] || !wordToken.MatchString(t.text) || i+1 >= len(toks) || !objectWords[strings.ToLower(toks[i+1].text)] {
		return false
	}
	if i > 0 && !verbLead[strings.ToLower(toks[i-1].text)] && !strings.HasPrefix(toks[i-1].tag, "RB") {
		return false
	}
	acronym := strings.ToUpper(t.text) == t.text && strings.ToLower(t.text) != t.text
	return strings.HasPrefix(t.tag, "NN") || acronym || (strings.HasPrefix(t.tag, "VB") && i > 0 && t.text[0] >= 'A' && t.text[0] <= 'Z')
}

// quotedSpeech: quotations of four or more words are someone else's wording, not the author's.
var quotedSpeech = regexp.MustCompile(`["“][^"”]*(?:\s[^"”\s]+){3,}["”]`)

// wordyPhrase: the stock phrases md-wordy-phrase names; Jev only judges whether a found one is
// the author's own prose and replaceable. (A hype-word list did worse than the paragraph rule.)
var (
	wordyPhrase = regexp.MustCompile(`(?i)\b(in order (?:to|for)|due to the fact that|owing to the fact that|at this point in time|at the present time|(?:is|are|was|were) able to|in the event that|a large number of|the majority of|for the (?:sole )?purpose of|in spite of the fact that|despite the fact that|with regard to|in regard to|prior to|subsequent to|has the ability to|have the ability to|make use of|in the near future|on a (?:daily|weekly|monthly|regular) basis|it is important to note that)\b`)
)

// markSpans: one unit per regexp match in s, with the match marked. lineOf, when set, gives the
// line of a byte offset in s, so each unit is anchored to the line its match is on.
func markSpans(kind string, re *regexp.Regexp, s string, base Unit, lineOf func(int) int) []Unit {
	var out []Unit
	for _, m := range re.FindAllStringIndex(s, -1) {
		if before := strings.ToLower(s[max(0, m[0]-6):m[0]]); strings.Contains(before, "not ") || strings.Contains(before, "n't ") {
			continue // "does not have the ability to" has no one-word swap
		}
		u := base
		u.Kind, u.Text = kind, s[:m[0]]+"⟦"+s[m[0]:m[1]]+"⟧"+s[m[1]:]
		if lineOf != nil {
			u.Start, u.End = lineOf(m[0]), lineOf(m[1]-1)
		}
		out = append(out, u)
	}
	return out
}

// withoutQuotes drops quoted speech from s and maps a byte offset in the result back to s.
func withoutQuotes(s string) (string, func(int) int) {
	locs := quotedSpeech.FindAllStringIndex(s, -1)
	raw := quotedSpeech.ReplaceAllString(s, "")
	own := strings.TrimSpace(raw)
	lead := len(raw) - len(strings.TrimLeft(raw, " \t"))
	return own, func(off int) int {
		p, shift := off+lead, 0
		for _, l := range locs {
			if l[0]-shift > p {
				break
			}
			shift += l[1] - l[0]
		}
		return p + shift
	}
}

// candidateUnits: per-word and per-phrase units for one prose block.
func candidateUnits(u Unit, header, heading string) []Unit {
	ctx := header + "\nHEADING: " + heading + "\nPARAGRAPH:\n" + u.Text
	var out []Unit
	for _, sent := range blockSentences(u.Text) {
		s := sent.text
		toks := tagSentence(s)
		own, inS := withoutQuotes(s)
		at := Unit{File: u.File, Context: ctx}
		out = append(out, markSpans("WordyPhrase", wordyPhrase, own, at, func(off int) int { return u.Start + sent.lineAt(inS(off)) })...)
		for i := range toks {
			var kind string
			switch {
			case actorVerb(toks, i):
				kind = "ActorVerb"
			case nounVerb(toks, i):
				kind = "NounVerb"
			default:
				continue
			}
			if m, off := mark(s, toks, i); m != "" {
				ln := u.Start + sent.lineAt(off)
				out = append(out, Unit{Kind: kind, File: u.File, Start: ln, End: ln, Text: m, Context: ctx})
			}
		}
	}
	return slices.Clip(out)
}

var listItem = regexp.MustCompile(`^(\s*)(?:([-*+])|(\d+[.)]))\s+(.*)$`)

// itemShape: how a list item opens (link, bold label, code, "Label:") and what grammar follows,
// so lists whose items all share one shape are never asked about.
func itemShape(text string) string {
	t := strings.TrimSpace(text)
	if prefix := itemPrefix(t); prefix != "plain" {
		return prefix // label-then-detail lists are parallel by their labels
	}
	plain := mdSentences(t)
	if len(plain) == 0 {
		return "plain"
	}
	toks := tagSentence(plain[0])
	class, finite := "other", false
	if len(toks) > 0 {
		class = openingClass(toks[0].tag)
	}
	for _, x := range toks[min(1, len(toks)):] {
		finite = finite || finiteTags[x.tag]
	}
	return fmt.Sprintf("%s/%v", class, finite)
}

var finiteTags = toSet("VBZ VBD VBP MD")

// itemPrefix: the markup an item opens with: link, bold, code, a short "Label:", or plain.
func itemPrefix(t string) string {
	switch {
	case strings.HasPrefix(t, "["):
		return "link"
	case strings.HasPrefix(t, "**") || strings.HasPrefix(t, "__"):
		return "bold"
	case strings.HasPrefix(t, "`"):
		return "code"
	}
	if k := strings.Index(t, ":"); k > 0 && len(strings.Fields(t[:k])) <= 4 {
		return "label"
	}
	return "plain"
}

var openingClasses = map[string]string{
	"VB": "command", "VBP": "command", "VBG": "gerund",
	"JJ": "noun", "DT": "noun", "CD": "noun",
	"PRP": "statement", "EX": "statement", "VBZ": "statement", "VBD": "statement",
}

// openingClass: the grammar an item's first word starts, from its part-of-speech tag.
func openingClass(tag string) string {
	if strings.HasPrefix(tag, "NN") {
		return "noun"
	}
	return firstNonEmpty(openingClasses[tag], "other")
}

type listEntry struct {
	start, end int
	lines      []string
	bodies     []string
}

// CommonMark tab stop width used when comparing mixed tab/space list indentation.
const tabColumns = 4

func indentWidth(ws string) int {
	w := 0
	for _, r := range ws {
		if r == '\t' {
			w += tabColumns
		} else {
			w++
		}
	}
	return w
}

// topLevelItems groups a Markdown list into its top-level items (preserving nested
// lines under their parent). When numberedOnly is true, only numbered items start entries.
func topLevelItems(text string, numberedOnly bool) []listEntry {
	var out []listEntry
	indent := -1
	ls := strings.Split(text, "\n")
	for i, l := range ls {
		m := listItem.FindStringSubmatch(l)
		isItem := m != nil && (!numberedOnly || m[3] != "")
		if isItem && (indent < 0 || indentWidth(m[1]) <= indent) {
			indent = indentWidth(m[1])
			if len(out) > 0 {
				out[len(out)-1].end = i - 1
			}
			out = append(out, listEntry{start: i, lines: []string{l}, bodies: []string{m[4]}})
		} else if len(out) > 0 {
			last := &out[len(out)-1]
			last.lines = append(last.lines, l)
			last.bodies = append(last.bodies, strings.TrimSpace(l))
		}
	}
	if len(out) > 0 {
		out[len(out)-1].end = len(ls) - 1
	}
	return out
}

// mixedList: a copy of a list unit for list-shape rules, only when its top-level items differ in shape.
func mixedList(u Unit) (Unit, bool) {
	items := topLevelItems(u.Text, false)
	shapes := make([]string, 0, len(items))
	for _, it := range items {
		shapes = append(shapes, itemShape(strings.Join(it.bodies, " ")))
	}
	if len(shapes) < 2 || len(slices.Compact(slices.Sorted(slices.Values(shapes)))) < 2 {
		return Unit{}, false
	}
	u.Kind = "MixedList"
	return u, true
}

// stepUnits splits a Steps block into one Step unit per top-level numbered item,
// anchored to its own line span with the full list in Context for the veto check.
func stepUnits(u Unit) []Unit {
	items := topLevelItems(u.Text, true)
	lastLine := strings.Count(u.Text, "\n")
	// Inherited block-level headers (HEADING, PREVIOUS BLOCK, POSITION) apply to every step;
	// NEXT LINE follows the whole list, so relabel it for non-final steps before appending LIST.
	nonFinalCtx := u.Context
	if i := strings.LastIndex(nonFinalCtx, "\nNEXT LINE: "); i >= 0 {
		nonFinalCtx = nonFinalCtx[:i] + "\nAFTER THE LIST: " + nonFinalCtx[i+len("\nNEXT LINE: "):]
	}
	listSuffix := "\nLIST:\n" + u.Text
	out := make([]Unit, 0, len(items))
	for _, it := range items {
		baseCtx := u.Context
		if it.end < lastLine {
			baseCtx = nonFinalCtx
		}
		out = append(out, Unit{
			Kind: "Step", File: u.File,
			Start: u.Start + it.start, End: u.Start + it.end,
			Text: strings.Join(it.lines, "\n"), Context: baseCtx + listSuffix,
		})
	}
	return out
}
