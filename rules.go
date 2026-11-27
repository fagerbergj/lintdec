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
	{"test-ignored-no-reason", "skipped test without ticket or guard", []string{"TestFunction"}, "whole",
		"The test is unconditionally skipped or disabled without naming a tracking ticket, owner, or concrete re-enable condition, leaving dead test coverage behind.",
		"TSR Test Smell Catalog, Ignored Test: skipped or disabled tests rot permanently when left untracked https://tsr-catalog.readthedocs.io/en/latest/smells/ignored-test.html"},
	{"test-over-mocked", "test mocks away the behavior it claims to verify", []string{"TestFunction"}, "whole",
		"The test mocks out the production logic it claims to test, or asserts only internal mock call choreography instead of exercising real behavior and asserting observable outputs or state.",
		"Google SWE Book, Ch. 11 Testing Overview: test via public APIs and verify state rather than brittle mock interactions https://abseil.io/resources/swe-book/html/ch11.html"},
	{"test-shared-state", "test mutates shared external or global state", []string{"TestFunction"}, "whole",
		"The test writes to a fixed shared filesystem path, binds a fixed port, or mutates shared global or environment state without isolating or restoring it, allowing sibling or parallel tests to interfere. Passing a loopback URL string to a pure parser without binding a port, or restoring mutations in a teardown hook (such as afterEach, defer, or monkeypatch), is not a finding.",
		"Google SWE Book, Ch. 11 Testing Overview: \"Tests should assume as little as possible about the outside environment\" https://abseil.io/resources/swe-book/html/ch11.html"},
	{"suppression-no-reason", "linter or type suppression without rationale", []string{"Directive"}, "whole",
		"The linter, compiler, or type-checker suppression disables a check without stating a concrete technical reason why the suppressed code is safe or intentional. A suppression inside a test error-expectation block (such as pytest.raises or toThrow) where the test intentionally triggers a type or expression check to assert that it raises is not a finding.",
		"eslint-comments/require-description & Hu et al., FSE 2025 (DOI 10.1145/3715729): unexplained suppressions accumulate obsolete bypasses and mask real defects https://eslint-community.github.io/eslint-plugin-eslint-comments/rules/require-description.html"},
	{"comment-long", "comment longer than it earns", []string{"Comment"}, "whole",
		"The comment runs into a multi-paragraph essay, or turns a short note into a long passage, where a decision record, a doc link, or nothing would do.",
		"Google C++ Style Guide, Function Comments: \"However, do not be unnecessarily verbose or state the completely obvious.\" https://google.github.io/styleguide/cppguide.html#Function_Comments"},
	{"comment-tautology", "comment restates the code", []string{"Comment"}, "whole",
		"The comment merely repeats what the adjacent declaration's name or code already states directly, adding no reason, constraint, or non-obvious behavior. If any clause states a mechanism, constraint, or behavior the name and code do not state directly, it is not tautological.",
		"Google Testing Blog, To Comment or Not to Comment? (2017): \"avoid comments that just repeat what the code does. These are just noise\" https://testing.googleblog.com/2017/07/code-health-to-comment-or-not-to-comment.html"},
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
	{"md-two-names", "one thing called by two names", []string{"Document"}, "whole",
		"The document calls the same thing (a component, service, field, step, team, or concept) by two or more different names without saying they are the same, so a reader could think they are different things.",
		"T-01 · Microsoft style, Writing tips: \"Use one word for a concept, and use it consistently.\" https://learn.microsoft.com/en-us/style-guide/global-communications/writing-tips"},
	{"md-inanimate-actor", "thing described doing what only a person can", []string{"ActorVerb"}, "local",
		"The marked verb in the TARGET sentence has a thing (a program, file, process, document, idea, or situation) as its subject, not a person or animal, and the verb describes something only a person or animal can literally do (eat, swallow, bite, want, know, decide, hide, refuse, complain, lie), so the reader must translate a figure of speech into what actually happens.",
		"V-11 · Clark, Martin & Mahowald, Why is AI-generated language so hard to understand? (2026): inanimate subjects with animate verbs \"violate a selectional restriction\" and are overused by frontier models https://arxiv.org/abs/2610.29162"},
	{"md-wordy-phrase", "several words where one will do", []string{"WordyPhrase"}, "local",
		"The marked phrase in the TARGET sentence is a stock wordy phrase that one short word replaces with no change of meaning (in order to: to; due to the fact that: because; is able to: can; in the event that: if; prior to: before; for the purpose of: to or for; make use of: use).",
		"C-01 · Google Technical Writing One, Short sentences: table of wordy phrases, \"at this point in time | now\", \"is able to | can\" https://developers.google.com/tech-writing/one/short-sentences"},
	{"md-prose-list", "list buried in a sentence", []string{"Paragraph"}, "local",
		"A sentence packs three or more parallel items, options, or steps inline (\"create, update, publish, and delete\"; \"first X, then Y, then Z\") where a bulleted or numbered list would be easier to scan.",
		"C-10 · Google Technical Writing One, Lists and tables: \"Generally speaking, embedded lists are a poor way to present technical information.\" https://developers.google.com/tech-writing/one/lists-and-tables"},
	{"md-list-not-parallel", "list items do not match each other", []string{"MixedList"}, "local",
		"The items of one list do not match in grammar or kind: some are commands and others noun phrases or full sentences, some active and others passive, or one item belongs to a different category from the rest.",
		"S-11 · Google Technical Writing One, Lists and tables: \"Effective lists are parallel; defective lists tend to be nonparallel.\" https://developers.google.com/tech-writing/one/lists-and-tables"},
	{"md-blames-reader", "text blames the reader", []string{"Paragraph", "Bullet", "Steps"}, "local",
		"A sentence tells the reader they made a mistake or did something wrong (\"you failed to\", \"you forgot\", \"your invalid input\", \"you should have\") instead of stating the problem and how to fix it.",
		"V-07 · Write the Docs, Style guides (error messages): \"Don't blame the user – be humble.\" https://www.writethedocs.org/guide/writing/style-guides/"},
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
	{"md-no-remediation", "error or troubleshooting section omits how to fix it", []string{"Section"}, "whole",
		"The troubleshooting, warning, or error section describes a failure or symptom without stating a concrete remediation step, workaround, or diagnostic action the reader can take. An API or architecture reference table that only enumerates error types or status codes is not a finding.",
		"OpenSSF Scorecard Check Docs & Google Tricorder: error and check docs must pair each failure with actionable remediation steps https://github.com/ossf/scorecard/blob/main/docs/checks.md"},
	{"unbounded-retry", "retry loop lacks cap, backoff, or error classification", []string{"Function", "Script"}, "whole",
		"The retry logic retries a failed operation without bounding the number of attempts, without backing off between retries, or without distinguishing transient failures from permanent ones. A loop that paginates a result set, polls for a state change, drains a work queue, or builds a config is not retrying failed attempts and is not a finding.",
		"Stoica et al., SOSP 2024, Analyzing Retry Incidents in Cloud Systems: unbounded or unclassified retries cause retry storms and cascading failures https://jonathanmace.github.io/papers/stoica2024retry.pdf"},
	{"hand-rolled-stdlib", "function hand-rolls a standard library primitive", []string{"Function"}, "whole",
		"The function reimplements standard parsing, encoding, escaping, formatting, or utility logic (such as CSV, URL/query strings, Base64, UUID, HTML escaping, ISO-8601 dates, CLI flags, or deep equality) that the language's standard library already provides safely. Delegating the core work to the standard library (including calling it first with a runtime feature-detection fallback), or implementing a minimal subset, hot-path constraint, or zero-dependency requirement explicitly explained by a comment in TARGET or CONTEXT, is not a finding.",
		"Go modernize & Ruff FURB: hand-rolled reimplementations of standard library primitives miss edge cases and add maintenance burden https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/modernize"},
	{"lint-config-weakened", "linter config weakens rules or excludes code without rationale", []string{"LintConfig"}, "whole",
		"The linter configuration disables or downgrades correctness or maintainability rules globally, or excludes production source directories, without scoping the relaxation to tests or generated output or stating a concrete technical rationale.",
		"Google SWE Book, Ch. 20 Static Analysis & ESLint Rules Docs: permanent warning downgrades and unexplained exclusions silently hide defects https://abseil.io/resources/swe-book/html/ch20.html"},
}

// ruleLevel: each rule's fixed severity. Jev's score only decides whether a finding is reported.
var ruleLevel = map[string]string{
	"swallowed-error": "error", "flakey-test": "error",
	"tautology-test": "error", "test-shared-state": "error", "unbounded-retry": "error",
	"comment-contradicts-code": "warning", "name-mismatch": "warning", "stub-body": "warning", "incomplete-assertion": "warning", "iface-leaky": "warning",
	"test-ignored-no-reason": "warning", "test-over-mocked": "warning", "suppression-no-reason": "warning",
	"hand-rolled-stdlib": "warning", "lint-config-weakened": "warning",
	"file-several-jobs": "warning",
	"comment-long":      "info", "comment-tautology": "info", "comment-hedge": "info",
	"comment-history": "info", "todo-no-owner": "info",
	"md-two-names":              "warning",
	"md-wordy-phrase":           "info",
	"md-prose-list":             "info",
	"md-list-not-parallel":      "info",
	"md-blames-reader":          "warning",
	"md-inanimate-actor":        "info",
	"md-no-purpose":             "warning",
	"md-rfc-no-ask":             "warning",
	"md-adr-many-decisions":     "warning",
	"md-adr-no-cost":            "warning",
	"md-one-sided-alternatives": "info",
	"md-no-remediation":         "warning",
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
