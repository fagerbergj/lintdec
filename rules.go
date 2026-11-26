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
	Source  string // source, quote or gloss, URL: the published rule this one enforces; doc rules prefix a catalogue id
}

// allRulesVersion hashes the full catalogue for batched chat prompts;
// ruleVersions hashes one rule + its veto for single-rule systemone calls.
var (
	allRulesVersion string
	ruleVersions    map[string]string
)

func init() {
	all := sha256.New()
	all.Write([]byte(systemPrompt))
	ruleVersions = make(map[string]string, len(rules))
	for _, r := range rules {
		entry := r.ID + "\x00" + r.Summary + "\x00" + r.Rubric + "\x00" + r.Scope + "\x00" + ruleVetoes[r.ID]
		all.Write([]byte(entry))
		one := sha256.New()
		one.Write([]byte(systemPrompt + "\x00" + entry))
		ruleVersions[r.ID] = hex.EncodeToString(one.Sum(nil))
	}
	allRulesVersion = hex.EncodeToString(all.Sum(nil))
}

func ruleVersion(id string) string {
	if v, ok := ruleVersions[id]; ok {
		return v
	}
	h := sha256.New()
	h.Write([]byte(systemPrompt + "\x00" + id))
	return hex.EncodeToString(h.Sum(nil))
}

var rules = []Rule{
	// xunitpatterns.com answers nothing on 443 (TLS refused direct and via proxy), so
	// this stays http; the guard tests "://" for exactly this citation.
	{"flakey-test", "test relies on timing or scheduling", []string{"TestFunction"}, "local",
		"The test can pass or fail depending on machine speed or scheduling: time.Sleep used to settle state, polling loops against real deadlines, comparisons involving time.Now() or elapsed durations, map or set iteration order assumed stable, or goroutines assumed to have finished.",
		"Gerard Meszaros, xUnit Test Patterns (Erratic Test): \"One or more tests are behaving erratically; sometimes they pass and sometimes they fail.\" http://xunitpatterns.com/Erratic%20Test.html"},
	{"tautology-test", "test that cannot fail", []string{"TestFunction"}, "whole",
		"The test cannot distinguish a correct implementation from a broken one: it only checks that nothing panics, asserts on values it constructed itself, asserts a property that holds regardless of the code under test, or every assertion is true by construction.",
		"Google Engineering Practices, Code Review (Tests): \"Will the tests actually fail when the code is broken?\" https://google.github.io/eng-practices/review/reviewer/looking-for.html#tests"},
	{"incomplete-assertion", "important result goes unasserted", []string{"TestFunction"}, "whole",
		"An important part of the result is not asserted: a (value, error) return where only one is checked, a struct where only some fields are compared after the operation, or an error case asserted without asserting the behavior the test name promises.",
		"Google Engineering Practices, Code Review (Tests): \"Does each test make simple and useful assertions?\" https://google.github.io/eng-practices/review/reviewer/looking-for.html#tests"},
	{"comment-long", "comment longer than it earns", []string{"Comment"}, "whole",
		"The comment runs into a multi-paragraph essay, or turns a short note into a long passage, where a decision record, a doc link, or nothing would do.",
		"Google C++ Style Guide, Function Comments: \"However, do not be unnecessarily verbose or state the completely obvious.\" https://google.github.io/styleguide/cppguide.html#Function_Comments"},
	{"comment-tautology", "comment restates the code", []string{"Comment"}, "whole",
		"The comment merely repeats what the adjacent declaration's name or code already states directly, adding no reason, constraint, or non-obvious behavior. If any clause states a mechanism, constraint, or behavior the name and code do not state directly, it is not tautological.",
		"Google Testing Blog, To Comment or Not to Comment? (2017): \"avoid comments that just repeat what the code does. These are just noise\" https://testing.googleblog.com/2017/07/code-health-to-comment-or-not-to-comment.html"},
	{"comment-excuse", "apology or deferral for a code smell", []string{"Comment"}, "whole",
		"The comment apologizes for or defends the code, or defers a known problem, without naming an owner, a ticket, or a concrete plan.",
		"Kernighan & Plauger, The Elements of Programming Style (2nd ed., 1978), carried in Henney, Comment Only What the Code Cannot Say (ACCU Overload 157): \"Don't comment bad code - rewrite it.\" https://accu.org/journals/overload/28/157/henney_2796/"},
	{"comment-contradicts-code", "comment contradicts the code it describes", []string{"Comment"}, "whole",
		"A specific claim in the comment (an identifier, value, order of steps, return or error) is contradicted by code shown in the TARGET or CONTEXT. Code that is not shown is never evidence: if the claim cannot be checked against visible code, this is not a finding.",
		"Wen, Nagy, Bavota & Lanza, Code-Comment Inconsistencies (ICPC 2019): comments out of step with the code they describe are a widespread, measured failure mode https://doi.org/10.1109/ICPC.2019.00019"},
	{"comment-history", "comment narrates history instead of current behavior", []string{"Comment"}, "whole",
		"The comment describes past versions or changes (what the code used to do, what was changed or fixed, or which bug led here) rather than what the code does now or the constraint that binds it.",
		"Kevlin Henney, Comment Only What the Code Cannot Say (ACCU Overload 157): \"Version-related comments and commented-out code try to address questions of versioning and history. These questions have already been answered (far more effectively) by version control tools.\" https://accu.org/journals/overload/28/157/henney_2796/"},
	{"comment-hedge", "uncertainty left as a comment", []string{"Comment"}, "whole",
		"The comment hedges about the code's correctness or behavior, leaving an open question in the source instead of resolving it or tracking it.",
		"Google C++ Style Guide, TODO Comments: deferred work must name \"a very specific date\" or \"a very specific event\", so an open question is tracked rather than left as doubt https://google.github.io/styleguide/cppguide.html#TODO_Comments"},
	{"todo-no-owner", "TODO without an owner or ticket", []string{"Comment"}, "whole",
		"A TODO, FIXME, HACK, or XXX with no owner name, ticket reference (such as #123 or PROJ-123), or date, so nobody can tell who acts on it or whether it is still live.",
		"Google C++ Style Guide, TODO Comments: \"Use TODO comments for code that is temporary, a short-term solution, or good-enough but not perfect\" with a reference to a bug or owner https://google.github.io/styleguide/cppguide.html#TODO_Comments"},
	{"iface-leaky", "interface leaks its implementation", []string{"Interface"}, "whole",
		"The interface exposes implementation internals: parameters or results that are storage, ORM, transport, or framework types callers should not see, or methods that only make sense inside the implementation.",
		"David L. Parnas, On the Criteria To Be Used in Decomposing Systems into Modules (CACM 1972): split modules on the design decisions likely to change; \"Each module is then designed to hide such a decision from the others.\" https://doi.org/10.1145/361598.361623"},
	{"iface-god-method", "method signature doing too much", []string{"Interface"}, "whole",
		"A method takes so many parameters, or a bag of mostly-ignored options and callbacks, that most callers pass zero values; the signature should be split or take one options struct.",
		"Robert C. Martin, The Interface Segregation Principle (C++ Report, 1996): \"Clients should not be forced to depend upon interfaces that they do not use.\" https://objectmentor.com/resources/articles/isp.pdf"},
	{"file-several-jobs", "file doing more than one job", []string{"File"}, "whole",
		"The declaration list shows two or more groups of declarations whose names serve unrelated purposes, each of which would stand alone as its own file. Size alone is not a finding.",
		"Google Engineering Practices, Code Review (Design): \"Do the interactions of various pieces of code in the CL make sense?\" https://google.github.io/eng-practices/review/reviewer/looking-for.html#design"},
	{"name-mismatch", "name promises the opposite of the behavior", []string{"Function"}, "whole",
		"The name contradicts what the body actually does: a predicate name (is/has/contains) returning a non-boolean, a count returning a collection, a getter with a visible side effect, a parser or normalizer that mutates its input, a validation function that returns data instead of an error. Flag only when the name promises the opposite of the behavior; an imprecise or vague name that is not contradicted is not a finding.",
		"Google Engineering Practices, Code Review (Naming): \"A good name is long enough to fully communicate what the item is or does, without being so long that it becomes hard to read.\" https://google.github.io/eng-practices/review/reviewer/looking-for.html#naming"},
	{"swallowed-error", "catch drops an error the caller needs", []string{"Function", "Script"}, "whole",
		"A failure is handled in a way that loses what the caller needs: a recoverable error logged without the context needed to act on it and then dropped, a catch that swallows a real failure and reports success, or a rethrow that discards the original error and its cause. Ignoring an error in an idiomatic cleanup (deferred close, best-effort teardown), a deliberate documented fallback, or a test double under a test path that fakes or injects failures is not a finding.",
		"Go Code Review Comments, Handle Errors: \"Do not discard errors using _ variables. If a function returns an error, check it to make sure the function succeeded.\" https://go.dev/wiki/CodeReviewComments#handle-errors"},
	{"stub-body", "body is a placeholder for promised behavior", []string{"Function"}, "whole",
		"The body does not do what the name promises: empty, only a comment, throws not-implemented, or returns a zero value while the name promises computed work. Interface implementations whose contract says unimplemented, scaffolding explicitly marked as a stub, and test doubles (fakes or no-op implementations under a test path) that return empty values or fail on calls the test never makes are not findings.",
		"Google Engineering Practices, Code Review (Functionality): \"Does this CL do what the developer intended?\" https://google.github.io/eng-practices/review/reviewer/looking-for.html#functionality"},
	{"md-heading-promise", "section does not deliver what its heading promises", []string{"Section"}, "whole",
		"The heading names a specific topic, task, or question, and the text under it is mostly about something else, answers a different question, or leaves the named topic out.",
		"H-02 · Google style, Headings: \"Use descriptive headings and titles because they help a reader navigate their browser and the page.\" https://developers.google.com/style/headings"},
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
	{"md-prose-list", "list buried in a sentence", []string{"Paragraph"}, "local",
		"A sentence packs three or more parallel items, options, or steps inline (\"create, update, publish, and delete\"; \"first X, then Y, then Z\") where a bulleted or numbered list would be easier to scan.",
		"C-10 · Google Technical Writing One, Lists and tables: \"Generally speaking, embedded lists are a poor way to present technical information.\" https://developers.google.com/tech-writing/one/lists-and-tables"},
	{"md-prose-table", "prose that should be a table", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"The TARGET describes three or more things using the same two or more attributes each (for example each option's name, default, and effect), so a reader must compare them across sentences or list items instead of across table columns.",
		"C-11 · Microsoft style, Writing tips: \"Replace complex sentences and paragraphs with lists and tables.\" https://learn.microsoft.com/en-us/style-guide/global-communications/writing-tips"},
	{"md-topic-sentence", "paragraph's first sentence points the wrong way", []string{"Paragraph"}, "local",
		"The paragraph has three or more sentences, and its first sentence sets up a different subject from the one the rest of the paragraph is about, so a reader who reads only the first sentence gets the wrong idea.",
		"H-01 · Google Technical Writing One, Paragraphs: \"Good opening sentences establish the paragraph's central point.\" https://developers.google.com/tech-writing/one/paragraphs"},
	{"md-one-topic", "paragraph covers more than one topic", []string{"Paragraph"}, "local",
		"The paragraph covers two or more separate topics: at least one sentence is about a subject unrelated to the paragraph's main point and belongs in another paragraph or section.",
		"H-09 · Google Technical Writing One, Paragraphs: \"A paragraph should represent an independent unit of logic. Restrict each paragraph to the current topic.\" https://developers.google.com/tech-writing/one/paragraphs"},
	{"md-step-not-command", "numbered step that is not an instruction", []string{"Step"}, "local",
		"The TARGET is one numbered step from a procedural list, and it does not start with a command verb (Open, Run, Set) but instead describes a state, condition, or fact (\"The process stalls.\", \"The config file is in /etc.\").",
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
		"D-05 · Go proposal template: a proposal must carry \"A precise statement of the proposed change\" for reviewers to accept or decline https://go.googlesource.com/proposal/+/refs/heads/master/design/TEMPLATE.md"},
	{"md-adr-many-decisions", "decision record makes more than one decision", []string{"Document"}, "whole",
		"The document is an architecture decision record, and it records two or more independent decisions that could each be accepted or reversed on its own.",
		"D-06 · Martin Fowler, Architecture Decision Record: \"a short document that captures and explains a single decision\" https://martinfowler.com/bliki/ArchitectureDecisionRecord.html"},
	{"md-adr-no-cost", "decision record lists only benefits", []string{"Document"}, "whole",
		"The document is an architecture decision record, and its consequences name only benefits: no cost, downside, risk, or follow-up work the decision creates.",
		"D-08 · Martin Fowler, Architecture Decision Record: the rationale must summarize \"the trade-offs that were taken into account\" https://martinfowler.com/bliki/ArchitectureDecisionRecord.html"},
	{"md-one-sided-alternatives", "rejected alternatives get no fair hearing", []string{"Document"}, "whole",
		"The document records or proposes a decision, names alternatives it did not choose, and gives none of them a real advantage: each is listed only to be dismissed.",
		"D-09 · Martin Fowler, Architecture Decision Record: \"explicitly list all the serious alternatives that were considered, together with their pros and cons\" https://martinfowler.com/bliki/ArchitectureDecisionRecord.html"},
}

// ruleLevel: each rule's fixed severity. Jev's score only decides whether a finding is reported.
var ruleLevel = map[string]string{
	"swallowed-error": "error", "flakey-test": "error",
	"tautology-test":           "error",
	"comment-contradicts-code": "warning", "name-mismatch": "warning", "stub-body": "warning", "incomplete-assertion": "warning", "iface-leaky": "warning",
	"iface-god-method": "warning", "file-several-jobs": "warning",
	"comment-long": "info", "comment-tautology": "info", "comment-hedge": "info",
	"comment-excuse": "info", "comment-history": "info", "todo-no-owner": "info",
	"md-heading-promise":        "warning",
	"md-restates":               "info",
	"md-two-names":              "warning",
	"md-hidden-verb":            "info",
	"md-wordy-phrase":           "info",
	"md-prose-list":             "info",
	"md-prose-table":            "info",
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
