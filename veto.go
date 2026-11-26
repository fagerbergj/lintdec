package main

// Jev ignores exceptions folded into a rubric ("X, unless Y"), so each rule's exception is asked
// as its own isolated question when the primary rule triggers. Only the System One path asks them.
const vetoConfidence = 0.5

var ruleVetoes = map[string]string{
	"flakey-test":               "Would this test pass reliably even on a slow, heavily loaded machine: every sleep only simulates a delay or bounds a failure path, every deadline is generous, and no assertion depends on goroutine order or wall-clock speed?",
	"incomplete-assertion":      "Does the test assert every behavior its name or doc comment promises, even if it leaves incidental fields or return values unchecked?",
	"swallowed-error":           "Is any ignored or replaced error in TARGET deliberate and harmless: best-effort work, a value that cannot realistically fail, idiomatic cleanup, or a fallback the code or a comment documents?",
	"comment-contradicts-code":  "Is every claim in the comment that can be checked against the visible code consistent with that code?",
	"stub-body":                 "Is the minimal body intentional: a no-op or placeholder its name or comment describes, a generated or dependency-injection stub, or an implementation whose contract is to do nothing?",
	"iface-leaky":               "Is this interface a deliberately thin wrapper or test seam over a client or SDK, where exposing that client's types is the point?",
	"file-several-jobs":         "Do all the declarations serve one purpose, such as one command, one feature, or one test subject with its helpers?",
	"name-mismatch":             "Does the body do what its name says, read plainly?",
	"comment-hedge":             "Does the comment state a known fact or documented limitation rather than leave doubt about whether the code is correct?",
	"comment-history":           "Does the comment explain current behavior, a binding constraint, a live legacy data or schema format the code still handles, or an in-test scenario timeline (such as v1/v2 fixture steps), rather than narrating code or git history?",
	"comment-long":              "Does every sentence in the comment state a non-obvious constraint, invariant, or design decision that the code cannot express, rather than pad a short note into an essay?",
	"md-inanimate-actor":        "Is the subject of the marked verb in TARGET a person, a role (a PM, the caller, the recipient, the on-call engineer), a team or organization, or an AI model, agent, or assistant (an LLM, Claude, the agent)?",
	"md-wordy-phrase":           "Would replacing the marked phrase in TARGET with a single plain word change the meaning here (for example \"should be able to\", \"for the sole purpose of\", a negation, or a fixed user-story template), or is the marked phrase inside quoted text, a title, or code?",
	"md-two-names":              "Is each thing called by one name in the document's own prose, with any variants being ticket titles, file paths, code identifiers, or second names introduced as such where they first appear?",
	"md-prose-list":             "Are the inline items short words or names that read naturally in one sentence (such as \"Go, Kotlin, and TypeScript\"), or are the clauses one argument building to a conclusion, or is the sentence a fixed template?",
	"md-list-not-parallel":      "Do the items match in grammar and kind well enough that a reader would not notice a mismatch, for example all following the same label-then-detail pattern?",
	"md-blames-reader":          "Does the text describe the problem or the fix without saying the reader was at fault?",
	"md-no-purpose":             "Does the title or first paragraph tell the reader what the document is for?",
	"md-rfc-no-ask":             "Is the document something other than a proposal asking reviewers to approve a change, such as an idea, goal, or planning page, a README, guide, reference, notes, or a record of a decision already made?",
	"md-adr-many-decisions":     "Is the document something other than an architecture decision record, or does it record one decision with its parts?",
	"md-adr-no-cost":            "Is the document something other than an architecture decision record, or does it name at least one cost, downside, risk, or piece of follow-up work?",
	"md-one-sided-alternatives": "Does the document name no rejected alternatives, give at least one of them a real advantage, or is it something other than a decision document, such as a tracking table?",
	"test-ignored-no-reason":    "Is the test in TARGET skipped only conditionally for a runtime environment requirement (such as OS, missing optional credential, or short mode) or tied to a named issue or ticket?",
	"test-over-mocked":          "Do the mocks or fakes in TARGET only replace external boundaries (such as network, filesystem, clock, or third-party services) while exercising real production logic and asserting real outcomes?",
	"test-shared-state":         "Does the test in TARGET isolate every file, port, or environment change (using a temp directory helper, ephemeral port, or explicit restore/cleanup) so it cannot leak into sibling tests?",
	"suppression-no-reason":     "Is there a substantive explanatory comment in TARGET (whose leading lines may include the comment directly above the directive) or in SURROUNDING CONTEXT giving a concrete reason, rather than a bare directive or tautology like \"fix error\"?",
	"md-no-remediation":         "Does the section in TARGET either give concrete steps to fix or work around the failure, include a fenced code block placeholder (such as [sh code block] or [code block]) introduced by prose naming an action or fix the reader applies, link directly to remediation steps, or explicitly explain that no user action is required?",
	"unbounded-retry":           "Does the retry logic in TARGET cap the number of attempts and back off or delegate to a retry helper, OR is TARGET a pagination, polling, worker, or config-builder function rather than retrying failed attempts in an unbounded loop?",
	"hand-rolled-stdlib":        "Does TARGET call a standard library primitive to do the core parsing, encoding, or formatting, OR does a comment in TARGET or CONTEXT explicitly explain why the standard library cannot be used here (such as a documented minimal subset, hot-path allocation constraint, or zero-dependency requirement)?",
	"lint-config-weakened":      "Is every disabled rule, warning-only rule, or excluded path in TARGET either standard build/generated/fixture output (such as dist, build, .next, fixtures, node_modules), scoped to tests or stories, or accompanied by a comment explaining the technical reason?",
}

// ruleFloors: tuned per rule on labeled findings, with 30% of files held out; unlisted rules need 0.75.
var ruleFloors = map[string]float64{
	"todo-no-owner": 0.9, "comment-history": 0.85, "flakey-test": 0.8, "comment-long": 0.8,
	"comment-tautology": 0.7, "tautology-test": 0.7,
	"md-inanimate-actor":   0.6,
	"md-prose-list":        0.85,
	"md-list-not-parallel": 0.9,
	"md-no-purpose":        0.85,
}

var ruleVetoCuts = map[string]float64{"flakey-test": 0.3}

// defaultOff: opt-in rules (unfiltered high-cost code checks or repo-specific style checks).
var defaultOff = map[string]bool{
	"file-several-jobs": true, "incomplete-assertion": true, "name-mismatch": true,
	"stub-body": true, "swallowed-error": true, "comment-history": true, "md-two-names": true,
}

func ruleFloor(id string) float64 {
	if f, ok := ruleFloors[id]; ok {
		return f
	}
	return 0.75
}

func vetoID(id string) string { return id + "~veto" }

// vetoRule returns the exception question rule for r, if one is configured.
func vetoRule(r Rule) (Rule, bool) {
	q, ok := ruleVetoes[r.ID]
	if !ok {
		return Rule{}, false
	}
	return Rule{ID: vetoID(r.ID), Kinds: r.Kinds, Scope: "question", Rubric: q}, true
}

// reportable applies the effective confidence floor and exception after the cache,
// so threshold and floor tuning needs no new model calls.
func reportable(r Rule, v Verdict, floor float64, calibrate bool) bool {
	if !v.Lint || calibrate {
		return v.Lint
	}
	cut, ok := ruleVetoCuts[r.ID]
	if !ok {
		cut = vetoConfidence
	}
	return v.Confidence >= floor && (v.Veto == nil || *v.Veto < cut)
}
