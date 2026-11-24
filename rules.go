package main

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
)

// Rule is one decision the model makes per unit. Scope "local" rules may run
// on chunks of an oversized unit; "whole" rules get a line-numbered digest.
type Rule struct {
	ID      string
	Summary string
	Kinds   []string
	Scope   string // "local" | "whole" | "question" (an exception asked as written)
	Rubric  string
	Source  string // catalogue id, source, quote, URL: the published rule this one enforces
}

// promptVersion folds the system prompt and every rubric into one hash; the
// cache key carries it, so any prompt or rubric edit invalidates stale
// verdicts automatically.
var promptVersion = func() string {
	h := sha256.New()
	h.Write([]byte(systemPrompt))
	for _, r := range rules {
		h.Write([]byte(r.ID + "\x00" + r.Rubric + "\x00" + r.Scope + "\x00" + ruleVetoes[r.ID]))
	}
	return hex.EncodeToString(h.Sum(nil))
}()

var rules = []Rule{
	{"flakey-test", "test relies on timing or scheduling", []string{"TestFunction"}, "local",
		"The test can pass or fail depending on machine speed or scheduling: time.Sleep used to settle state, polling loops against real deadlines, comparisons involving time.Now() or elapsed durations, map or set iteration order assumed stable, or goroutines assumed to have finished.", ""},
	{"tautology-test", "test that cannot fail", []string{"TestFunction"}, "whole",
		"The test cannot distinguish a correct implementation from a broken one: it only checks that nothing panics, asserts on values it constructed itself, asserts a property that holds regardless of the code under test, or every assertion is true by construction.", ""},
	{"incomplete-assertion", "important result goes unasserted", []string{"TestFunction"}, "whole",
		"An important part of the result is not asserted: a (value, error) return where only one is checked, a struct where only some fields are compared after the operation, or an error case asserted without asserting the behavior the test name promises.", ""},
	{"wrong-assertion", "assertion contradicts the contract", []string{"TestFunction"}, "whole",
		"An assertion expects the opposite of what the code under test or its obvious contract gives: an error expected for valid input, success expected for invalid input, the wrong return value checked, or an expected value that contradicts what the tested code computes from the given inputs.", ""},
	{"test-except-pass", "test swallows all failures", []string{"TestFunction"}, "local",
		"The test cannot fail because failures are swallowed: a deferred recover that never fails the test, a recovery path that only logs, or a checked error overwritten before being reported so a failing branch reports success.", ""},
	{"assert-self", "assertion compares a value to itself", []string{"TestFunction"}, "local",
		"An assertion compares two expressions that hold the same value: a variable compared with itself, expected and actual derived from the same expression, or an expected literal clearly copied from the code under test rather than from the specification.", ""},
	{"comment-long", "comment longer than it earns", []string{"Comment"}, "whole",
		"The comment runs into a multi-paragraph essay, or turns a short note into a long passage, where a decision record, a doc link, or nothing would do.", ""},
	{"comment-tautology", "comment restates the code", []string{"Comment"}, "whole",
		"The comment merely narrates what the adjacent code already says, adding no information a reader could not get from the code in the same time. If any clause of the comment states a mechanism, constraint or behavior the code or name does not state, it is not tautological, even if another clause restates the name — for pure name restatement with added detail, comment-restates-name is the rule.", ""},
	{"comment-excuse", "apology or deferral for a code smell", []string{"Comment"}, "whole",
		"The comment apologizes for or defends the code, or defers a known problem, without naming an owner, a ticket, or a concrete plan.", ""},
	{"comment-contradicts-code", "comment contradicts the code it describes", []string{"Comment"}, "whole",
		"A specific claim in the comment (an identifier, value, order of steps, return or error) is contradicted by code shown in the TARGET or CONTEXT. Code that is not shown is never evidence: if the claim cannot be checked against visible code, this is not a finding.", ""},
	{"comment-history", "comment narrates history instead of current behavior", []string{"Comment"}, "whole",
		"The comment describes past versions or changes (what the code used to do, what was changed or fixed, or which bug led here) rather than what the code does now or the constraint that binds it.", ""},
	{"comment-hedge", "uncertainty left as a comment", []string{"Comment"}, "whole",
		"The comment hedges about the code's correctness or behavior, leaving an open question in the source instead of resolving it or tracking it.", ""},
	{"comment-restates-name", "doc comment that only repeats the name", []string{"Comment"}, "whole",
		"The comment's first line is the function or variable name in prose and the rest adds no contract: no invariants, no error cases, no units.", ""},
	{"todo-no-owner", "TODO without an owner or ticket", []string{"Comment"}, "whole",
		"A TODO, FIXME, HACK, or XXX with no owner name, ticket reference, or date, so nobody can tell who acts on it or whether it is still live.", ""},
	{"iface-leaky", "interface leaks its implementation", []string{"Interface"}, "whole",
		"The interface exposes implementation internals: parameters or results that are storage, ORM, transport, or framework types callers should not see, or methods that only make sense inside the implementation.", ""},
	{"iface-god-method", "method signature doing too much", []string{"Interface"}, "whole",
		"A method takes so many parameters, or a bag of mostly-ignored options and callbacks, that most callers pass zero values; the signature should be split or take one options struct.", ""},
	{"file-several-jobs", "file doing more than one job", []string{"File"}, "whole",
		"The declaration list shows two or more groups of declarations whose names serve unrelated purposes, each of which would stand alone as its own file. Size alone is not a finding.", ""},
	{"name-mismatch", "name promises the opposite of the behavior", []string{"Function"}, "whole",
		"The name contradicts what the body actually does: a predicate name (is/has/contains) returning a non-boolean, a count returning a collection, a getter with a visible side effect, a parser or normalizer that mutates its input, a validation function that returns data instead of an error. Flag only when the name promises the opposite of the behavior; an imprecise or vague name that is not contradicted is not a finding.", ""},
	{"swallowed-error", "catch drops an error the caller needs", []string{"Function"}, "whole",
		"A failure is handled in a way that loses what the caller needs: a recoverable error logged without the context needed to act on it and then dropped, a catch that swallows a real failure and reports success, or a rethrow that discards the original error and its cause. Ignoring an error in an idiomatic cleanup (deferred close, best-effort teardown), a deliberate documented fallback, or a test double under a test path that fakes or injects failures is not a finding.", ""},
	{"stub-body", "body is a placeholder for promised behavior", []string{"Function"}, "whole",
		"The body does not do what the name promises: empty, only a comment, throws not-implemented, or returns a zero value while the name promises computed work. Interface implementations whose contract says unimplemented, scaffolding explicitly marked as a stub, and test doubles (fakes or no-op implementations under a test path) that return empty values or fail on calls the test never makes are not findings.", ""},
	{"commented-code", "commented-out code left in source", []string{"Comment"}, "whole",
		"The comment block is code from an earlier or alternative version of this code, not prose: statements, control flow, or calls in comment form where the live code beside it does the same job differently. Illustrative snippets in documentation-style comments and commented example values are not findings.", ""},
	{"md-heading-promise", "section does not deliver what its heading promises", []string{"Section"}, "whole",
		"The heading names a specific topic, task, or question, and the text under it is mostly about something else, answers a different question, or leaves the named topic out.",
		"H-02 · Google style, Headings: \"Use descriptive headings and titles because they help a reader navigate their browser and the page.\" https://developers.google.com/style/headings"},
	{"md-announces", "sentence announces content instead of stating it", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"A sentence only says what the document or section is about to do, or that something is worth noting (\"This section describes...\", \"It is worth noting that...\", \"Below we explain...\", \"As mentioned above...\"), states no fact of its own, and could be deleted with no loss.",
		"C-05 · Microsoft style, Top 10 tips: \"Get to the point fast. Lead with what's most important.\" https://learn.microsoft.com/en-us/style-guide/top-10-tips-style-voice"},
	{"md-restates", "paragraph repeats what was just said", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"The TARGET repeats, in other words, what the HEADING or the PREVIOUS BLOCK in CONTEXT already said, adding no new fact, condition, example, or consequence.",
		"C-06 · GitLab style guide: \"Each section should add new information. Do not summarize what was just explained.\" https://docs.gitlab.com/development/documentation/styleguide/"},
	{"md-two-names", "one thing called by two names", []string{"Document"}, "whole",
		"The document calls the same thing (a component, service, field, step, team, or concept) by two or more different names without saying they are the same, so a reader could think they are different things.",
		"T-01 · Microsoft style, Writing tips: \"Use one word for a concept, and use it consistently.\" https://learn.microsoft.com/en-us/style-guide/global-communications/writing-tips"},
	{"md-inanimate-actor", "thing described doing what only a person can", []string{"ActorVerb"}, "local",
		"The marked verb in the TARGET sentence has a thing (a program, file, process, document, idea, or situation) as its subject, not a person or animal, and the verb describes something only a person or animal can literally do (eat, swallow, bite, want, know, decide, hide, refuse, complain, lie), so the reader must translate a figure of speech into what actually happens.",
		"V-11 · Clark, Martin & Mahowald, Why is AI-generated language so hard to understand? (2026): inanimate subjects with animate verbs \"violate a selectional restriction\" and are overused by frontier models https://arxiv.org/abs/2610.29162"},
	{"md-noun-as-verb", "noun pressed into service as a verb", []string{"NounVerb"}, "local",
		"The marked word in the TARGET sentence is normally a noun, a name, or an acronym (PR, calendar, footnote, Slack, ticket) but is used here as a verb that standard English does not have, so the reader must work out what action it means.",
		"P-02 · Clark, Martin & Mahowald, Why is AI-generated language so hard to understand? (2026): \"using a denominal verb in an ad-hoc way is marked (e.g., calendar it)\" https://arxiv.org/abs/2610.29162"},
	{"md-hidden-verb", "action buried in a noun", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"A sentence turns an action into a noun wrapped in a weak verb (\"provides a description of\", \"performs an evaluation of\", \"make a decision about\", \"is in charge of the management of\") where one plain verb (describes, evaluates, decides, manages) says the same thing.",
		"C-02 · Google Technical Writing One, Short sentences: \"The phrase provides a detailed description of reduces to the verb describes\" https://developers.google.com/tech-writing/one/short-sentences"},
	{"md-wordy-phrase", "several words where one will do", []string{"WordyPhrase"}, "local",
		"The marked phrase in the TARGET sentence is a stock wordy phrase that one short word replaces with no change of meaning (in order to: to; due to the fact that: because; is able to: can; in the event that: if; prior to: before; for the purpose of: to or for; make use of: use).",
		"C-01 · Google Technical Writing One, Short sentences: table of wordy phrases, \"at this point in time | now\", \"is able to | can\" https://developers.google.com/tech-writing/one/short-sentences"},
	{"md-two-ideas", "sentence carries two separate ideas", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"One sentence carries two unrelated ideas joined by \"and\", \"which\", \"while\", or a semicolon, where the second clause goes in a different direction from the first instead of extending it, and would read better as its own sentence.",
		"C-08 · Google Technical Writing One, Short sentences: \"Focus each sentence on a single idea, thought, or concept.\" https://developers.google.com/tech-writing/one/short-sentences"},
	{"md-prose-list", "list buried in a sentence", []string{"Paragraph"}, "local",
		"A sentence packs three or more parallel items, options, or steps inline (\"create, update, publish, and delete\"; \"first X, then Y, then Z\") where a bulleted or numbered list would be easier to scan.",
		"C-10 · Google Technical Writing One, Lists and tables: \"Generally speaking, embedded lists are a poor way to present technical information.\" https://developers.google.com/tech-writing/one/lists-and-tables"},
	{"md-prose-table", "prose that should be a table", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"The TARGET describes three or more things using the same two or more attributes each (for example each option's name, default, and effect), so a reader must compare them across sentences or list items instead of across table columns.",
		"C-11 · Microsoft style, Writing tips: \"Replace complex sentences and paragraphs with lists and tables.\" https://learn.microsoft.com/en-us/style-guide/global-communications/writing-tips"},
	{"md-idiom", "idiom or culture-bound phrase", []string{"Sentence"}, "local",
		"A phrase in the TARGET sentence means something other than its literal words, or depends on one culture's references (sports, folk sayings, slang): \"kill two birds with one stone\", \"out of the box\", \"ballpark figure\", \"move the needle\", \"low-hanging fruit\".",
		"P-04 · Microsoft style, Writing tips: \"Avoid idioms, colloquial expressions, and culture-specific references.\" https://learn.microsoft.com/en-us/style-guide/global-communications/writing-tips"},
	{"md-ambiguous-pronoun", "pronoun with more than one possible meaning", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"A pronoun (it, this, that, they, these, which) could refer to two or more nouns or ideas named before it, and choosing the wrong one changes what the reader would do or believe.",
		"P-14 · GitLab style guide, Avoid: \"Ambiguous pronouns like it.\" https://docs.gitlab.com/development/documentation/styleguide/"},
	{"md-dropped-words", "words dropped so the reader must guess", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"A sentence leaves out a subject, verb, or article the reader must guess to understand it (\"Ensure switch off before restart\", \"If installed, remove\", \"Config in repo root\"), and the guess could be wrong.",
		"P-18 · Microsoft style, Writing tips: \"Include articles, such as the. Articles help readers and translation software identify the nouns and modifiers in a sentence.\" https://learn.microsoft.com/en-us/style-guide/global-communications/writing-tips"},
	{"md-topic-sentence", "paragraph's first sentence points the wrong way", []string{"Paragraph"}, "local",
		"The paragraph has three or more sentences, and its first sentence sets up a different subject from the one the rest of the paragraph is about, so a reader who reads only the first sentence gets the wrong idea.",
		"H-01 · Google Technical Writing One, Paragraphs: \"Good opening sentences establish the paragraph's central point.\" https://developers.google.com/tech-writing/one/paragraphs"},
	{"md-one-topic", "paragraph covers more than one topic", []string{"Paragraph"}, "local",
		"The paragraph covers two or more separate topics: at least one sentence is about a subject unrelated to the paragraph's main point and belongs in another paragraph or section.",
		"H-09 · Google Technical Writing One, Paragraphs: \"A paragraph should represent an independent unit of logic. Restrict each paragraph to the current topic.\" https://developers.google.com/tech-writing/one/paragraphs"},
	{"md-step-not-command", "numbered step that is not an instruction", []string{"Steps"}, "local",
		"The TARGET is a numbered list of steps the reader performs, and at least one step does not start with a command verb (Open, Run, Set) but describes a state or something that happens (\"The process stalls.\", \"The config file is in /etc.\").",
		"S-13 · Google Technical Writing One, Lists and tables: \"Consider starting all items in a numbered list with an imperative verb.\" https://developers.google.com/tech-writing/one/lists-and-tables"},
	{"md-list-not-parallel", "list items do not match each other", []string{"MixedList"}, "local",
		"The items of one list do not match in grammar or kind: some are commands and others noun phrases or full sentences, some active and others passive, or one item belongs to a different category from the rest.",
		"S-11 · Google Technical Writing One, Lists and tables: \"Effective lists are parallel; defective lists tend to be nonparallel.\" https://developers.google.com/tech-writing/one/lists-and-tables"},
	{"md-marketing", "promotional language instead of facts", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"A sentence uses promotional or hyped wording the reader cannot check (\"blazing fast\", \"seamless\", \"world-class\", \"powerful\", \"best-in-class\", \"revolutionary\") instead of a factual claim.",
		"V-06 · Nielsen Norman Group, Concise, Scannable, and Objective: \"users detest anything that seems like marketing fluff or overly hyped language\" https://www.nngroup.com/articles/concise-scannable-and-objective-how-to-write-for-the-web/"},
	{"md-blames-reader", "text blames the reader", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"A sentence tells the reader they made a mistake or did something wrong (\"you failed to\", \"you forgot\", \"your invalid input\", \"you should have\") instead of stating the problem and how to fix it.",
		"V-07 · Write the Docs, Style guides (error messages): \"Don't blame the user – be humble.\" https://www.writethedocs.org/guide/writing/style-guides/"},
	{"md-howto-digression", "explanation interrupts a how-to", []string{"Section"}, "whole",
		"The section tells the reader how to do a task, and one or more of its paragraphs stops to explain concepts, history, or design rationale the reader does not need to complete the task.",
		"S-07 · Diátaxis, Quality: \"when a digression into explanation interrupts a how-to guide\" https://diataxis.fr/quality/"},
	{"md-no-purpose", "opening does not say what the document is for", []string{"Document"}, "whole",
		"The opening paragraphs, after the title, do not tell the reader what the document is for or what it covers, so a reader who reads only the first paragraph cannot tell whether the document is relevant to them.",
		"D-02 · Google Technical Writing One, Documents: \"Imagine that your peers might only read the first paragraph of your document.\" https://developers.google.com/tech-writing/one/documents"},
	{"md-rfc-no-ask", "proposal never states the decision it asks for", []string{"Document"}, "whole",
		"The document proposes a change for review (an RFC or design proposal), and never states the specific decision it asks reviewers to make or what approval would mean.",
		"D-05 · dotagents RFC authoring: \"the proposal answers who is affected, what changes, why now, and what approval means\" https://github.com/fagerbergj/dotagents/blob/44c8804/skills/rfc-authoring/SKILL.md"},
	{"md-adr-many-decisions", "decision record makes more than one decision", []string{"Document"}, "whole",
		"The document is an architecture decision record, and it records two or more independent decisions that could each be accepted or reversed on its own.",
		"D-06 · dotagents ADR authoring: \"it contains one clear decision and the correct local status\" https://github.com/fagerbergj/dotagents/blob/44c8804/skills/adr-authoring/SKILL.md"},
	{"md-adr-no-cost", "decision record lists only benefits", []string{"Document"}, "whole",
		"The document is an architecture decision record, and its consequences name only benefits: no cost, downside, risk, or follow-up work the decision creates.",
		"D-08 · dotagents ADR authoring: \"consequences include costs and follow-up work, not only benefits\" https://github.com/fagerbergj/dotagents/blob/44c8804/skills/adr-authoring/SKILL.md"},
	{"md-one-sided-alternatives", "rejected alternatives get no fair hearing", []string{"Document"}, "whole",
		"The document records or proposes a decision, names alternatives it did not choose, and gives none of them a real advantage: each is listed only to be dismissed.",
		"D-09 · Martin Fowler, Architecture Decision Record: \"explicitly list all the serious alternatives that were considered, together with their pros and cons\" https://martinfowler.com/bliki/ArchitectureDecisionRecord.html"},
}

// ruleLevel: each rule's fixed severity. Jev's score only decides whether a finding is reported.
var ruleLevel = map[string]string{
	"swallowed-error": "error", "flakey-test": "error",
	"wrong-assertion": "error", "tautology-test": "error", "test-except-pass": "error", "assert-self": "error",
	"comment-contradicts-code": "warning", "name-mismatch": "warning", "stub-body": "warning", "incomplete-assertion": "warning", "iface-leaky": "warning",
	"iface-god-method": "warning", "file-several-jobs": "warning",
	"comment-long": "info", "comment-tautology": "info", "comment-restates-name": "info", "comment-hedge": "info",
	"comment-excuse": "info", "comment-history": "info", "todo-no-owner": "info", "commented-code": "info",
	"md-heading-promise":        "warning",
	"md-announces":              "info",
	"md-restates":               "info",
	"md-two-names":              "warning",
	"md-hidden-verb":            "info",
	"md-wordy-phrase":           "info",
	"md-two-ideas":              "info",
	"md-prose-list":             "info",
	"md-prose-table":            "info",
	"md-idiom":                  "info",
	"md-ambiguous-pronoun":      "warning",
	"md-dropped-words":          "info",
	"md-topic-sentence":         "info",
	"md-one-topic":              "info",
	"md-step-not-command":       "info",
	"md-list-not-parallel":      "info",
	"md-marketing":              "info",
	"md-blames-reader":          "warning",
	"md-inanimate-actor":        "info",
	"md-noun-as-verb":           "info",
	"md-howto-digression":       "info",
	"md-no-purpose":             "warning",
	"md-rfc-no-ask":             "warning",
	"md-adr-many-decisions":     "warning",
	"md-adr-no-cost":            "warning",
	"md-one-sided-alternatives": "info",
}

func init() {
	for _, r := range rules {
		if severityRank(ruleLevel[r.ID]) < 0 {
			panic("rule " + r.ID + " has no level in ruleLevel")
		}
	}
}

func findRule(id string) (Rule, bool) {
	for _, r := range rules {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}

func ruleApplies(r Rule, kind string) bool {
	return slices.Contains(r.Kinds, kind)
}
