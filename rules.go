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
	Scope   string // "local" | "whole"
	Rubric  string
}

// ruleVersion folds the system prompt and one rule's rubric into a hash so
// editing a single rubric invalidates only that rule's cached verdicts.
func ruleVersion(r Rule) string {
	h := sha256.New()
	h.Write([]byte(systemPrompt + "\x00" + r.ID + "\x00" + r.Summary + "\x00" + r.Rubric + "\x00" + r.Scope))
	return hex.EncodeToString(h.Sum(nil))
}

var rules = []Rule{
	{"flakey-test", "test relies on timing or scheduling", []string{"TestFunction"}, "local",
		"The test can pass or fail depending on machine speed or scheduling: time.Sleep used to settle state, polling loops against real deadlines, comparisons involving time.Now() or elapsed durations, map or set iteration order assumed stable, or goroutines assumed to have finished. Deterministic in-process mock servers, awaited async calls, and sequential tests that reset shared test flags in finally blocks are not flaky."},
	{"tautology-test", "test that cannot fail", []string{"TestFunction"}, "whole",
		"The test cannot distinguish a correct implementation from a broken one: it only checks that nothing panics, asserts on values it constructed itself, asserts a property that holds regardless of the code under test, or every assertion is true by construction."},
	{"incomplete-assertion", "important result goes unasserted", []string{"TestFunction"}, "whole",
		"An important part of the result is not asserted: a (value, error) return where only one is checked, a struct where only some fields are compared after the operation, or an error case asserted without asserting the behavior the test name promises. Asserting an exit code or error status for a negative case without also asserting exact message wording, or verifying behavior via recorded mock calls, is not a finding."},
	{"wrong-assertion", "assertion contradicts the contract", []string{"TestFunction"}, "whole",
		"An assertion expects the opposite of what the code under test or its obvious contract gives: an error expected for valid input, success expected for invalid input, the wrong return value checked, or an expected value that contradicts what the tested code computes from the given inputs. Flag only when the contradiction is directly evident in the test code itself; do not assume unobserved helpers behave differently from what the test's own assertions show."},
	{"test-except-pass", "test swallows all failures", []string{"TestFunction"}, "local",
		"The test cannot fail because failures are swallowed: a deferred recover that never fails the test, a recovery path that only logs, or a checked error overwritten before being reported so a failing branch reports success."},
	{"assert-self", "assertion compares a value to itself", []string{"TestFunction"}, "local",
		"An assertion compares two expressions that hold the same value: a variable compared with itself, expected and actual derived from the same expression, or an expected literal clearly copied from the code under test rather than from the specification. Comparing the system's output against a test fixture constant or expected input data is normal and not a finding."},
	{"comment-long", "comment longer than it earns", []string{"Comment"}, "whole",
		"The comment runs into a multi-paragraph essay, or turns a short note into a long passage, where a decision record, a doc link, or nothing would do."},
	{"comment-tautology", "comment restates the code", []string{"Comment"}, "whole",
		"The comment merely narrates what the adjacent code already says, adding no information a reader could not get from the code in the same time. If any clause of the comment states a mechanism, constraint or behavior the code or name does not state, it is not tautological, even if another clause restates the name — for pure name restatement with added detail, comment-restates-name is the rule."},
	{"comment-excuse", "apology or deferral for a code smell", []string{"Comment"}, "whole",
		"The comment apologizes for or defends the code, or defers a known problem, without naming an owner, a ticket, or a concrete plan."},
	{"comment-stale", "comment describes code that no longer does that", []string{"Comment"}, "whole",
		"Compared with the surrounding code, the comment describes behavior the code no longer performs: it names identifiers absent from the context, explains logic the code no longer performs, or states an outcome the code now contradicts."},
	{"comment-decision-record", "design comment that should be a decision record", []string{"Comment"}, "whole",
		"The comment discusses a non-obvious design choice but describes what the code does or narrates history, instead of recording the decision: what was chosen, which alternative was rejected and why, and the constraint that binds future changes. Flag only comments about a real design tradeoff, not routine code."},
	{"comment-hedge", "uncertainty left as a comment", []string{"Comment"}, "whole",
		"The comment hedges about the code's correctness or behavior, leaving an open question in the source instead of resolving it or tracking it."},
	{"comment-restates-name", "doc comment that only repeats the name", []string{"Comment"}, "whole",
		"The comment's first line is the function or variable name in prose and the rest adds no contract: no invariants, no error cases, no units."},
	{"todo-no-owner", "TODO without an owner or ticket", []string{"Comment"}, "whole",
		"A TODO, FIXME, HACK, or XXX with no owner name, ticket reference, or date, so nobody can tell who acts on it or whether it is still live."},
	{"md-para-long", "paragraph too long to scan", []string{"Paragraph"}, "local",
		"A paragraph packs more than one separate topic in a single block, where a reader must reorient between topics, and shorter paragraphs or a list would read at once. Sentences and clauses that develop the same point are one point, no matter how long the paragraph runs: length alone, without distinct topics, is not a violation, and a single sentence explaining one concept is not a violation."},
	{"md-hard-wrap", "prose hard-wrapped mid-sentence", []string{"Paragraph", "Bullet"}, "local",
		"Flag a paragraph or list item that is hard-wrapped to a fixed width mid-sentence: most of its lines end in the middle of a sentence (the line has no sentence-ending punctuation and the next line continues the same sentence) at roughly the same column. Lines that each hold a complete sentence — even at similar lengths — are normal markdown and must not be flagged; at least two mid-sentence line breaks are required, and a single-line unit cannot be hard-wrapped. Do not flag intentional blank-line paragraphing, single-line items, or wrapped command examples."},
	{"md-table-dense", "table cell carrying too much text", []string{"Table"}, "local",
		"Flag table cells that carry a paragraph or essay of dense prose — packing multiple distinct topics or procedural steps inside one cell where the information would read better as prose, a footnote, or a dedicated section. Judge the file's role from its path and headings: reference and working documents — CLI command references, API and protocol references, RFCs, SRS and audit records, design docs, hardware or network inventories, build notes — legitimately pack structured facts into cells, and their tables are the format, not a defect; do not flag them. The target is reader-facing prose documents (public READMEs, tutorials, changelogs, user guides) where a cell carries an essay the reader must parse inline. Do not flag header rows, short one-or-two-sentence command descriptions, or cells whose length comes from many short parallel entries — a list of values, actions or options, each with at most one short qualifier, is the reference form of such tables and is never dense."},
	{"md-jargon", "unintroduced terms a reader needs", []string{"Paragraph", "Bullet"}, "local",
		"Flag only when the paragraph relies on a term its own reader needs to follow its point, and the file neither defines nor introduces it. A term defined, introduced or explained anywhere in the same file — including later sections — is not jargon, even when the using paragraph comes before the definition, and a product or project's own names for its components are not jargon in that project's own documentation. Judge the audience from the file itself (name, headings, links): operational and personal notes, hardware or LLM configuration, patch notes, game or league rules, and internal design documents legitimately use their domain's vocabulary dense, and standard terms of the evident audience (an engineer reading engineering docs, an operator reading setup notes) are never jargon. Do not flag terms defined or referenced within the file, proper nouns used correctly, or a single unfamiliar term the paragraph still lets a competent reader follow. The target is prose aimed at a broader reader that presumes knowledge it never provides, with two or more such unexplained terms in one passage."},
	{"iface-leaky", "interface leaks its implementation", []string{"Interface"}, "whole",
		"The interface exposes implementation internals: parameters or results that are storage, ORM, transport, or framework types callers should not see, or methods that only make sense inside the implementation."},
	{"iface-god-method", "method signature doing too much", []string{"Interface"}, "whole",
		"A method takes so many parameters, or a bag of mostly-ignored options and callbacks, that most callers pass zero values; the signature should be split or take one options struct."},
	{"complex-method", "method doing several jobs", []string{"Function", "TestFunction"}, "whole",
		"The method is genuinely long and hard to follow as one unit because it tangles unrelated jobs together or buries its control flow in deeply nested loops and conditionals that a human would break up with early returns or extracted helpers. Compact functions (even with dense boolean expressions, array pipelines, or a few sequential steps of one job such as validate, fetch, and format), concise test cases exercising one feature's paths sequentially, and factory functions returning an object of small methods over shared closure helpers are not complex. Judge the method as a whole."},
	{"big-file", "file covering several topics", []string{"File"}, "whole",
		"Looking at the declaration list, the file groups more than one topic: distinct components or concerns whose names and helpers would read better as separate files. Flag only when the boundary between topics is obvious from the names."},
	{"name-mismatch", "name promises the opposite of the behavior", []string{"Function"}, "whole",
		"The name contradicts what the body actually does: a predicate name (is/has/contains) returning a non-boolean, a count returning a collection, a getter with a visible side effect, a parser or normalizer that mutates its input, a validation function that returns data instead of an error. Flag only when the name promises the opposite of the behavior; an imprecise or vague name that is not contradicted is not a finding."},
	{"swallowed-error", "catch drops an error the caller needs", []string{"Function"}, "whole",
		"A failure is handled in a way that loses what the caller needs: a recoverable error logged without context and then dropped, a catch that swallows a real failure and reports success or a bogus default, or a catch that rethrows a generic message losing what failed. Ignoring an error in an idiomatic cleanup (deferred close, best-effort teardown), a deliberate documented fallback, or translating a parse or validation failure into a clear domain error thrown to the caller is not a finding."},
	{"stub-body", "body is a placeholder for promised behavior", []string{"Function"}, "whole",
		"The body does not do what the name promises: empty, only a comment, throws not-implemented, or returns a zero value while the name promises computed work. Interface implementations whose contract says unimplemented, and scaffolding explicitly marked as a stub, are not findings."},
	{"commented-code", "commented-out code left in source", []string{"Comment"}, "whole",
		"The comment block is code from an earlier or alternative version of this code, not prose: statements, control flow, or calls in comment form where the live code beside it does the same job differently. Illustrative snippets in documentation-style comments and commented example values are not findings."},
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
