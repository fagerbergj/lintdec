package main

// Jev ignores exceptions folded into a rubric ("X, unless Y"), so each rule's exception is asked as its own
// question in the same call. Only the System One path asks them; chat models are gated by floors alone.
const vetoConfidence = 0.5

var ruleVetoes = map[string]string{
	"flakey-test":               "Would this test pass reliably even on a slow, heavily loaded machine: every sleep only simulates a delay or bounds a failure path, every deadline is generous, and no assertion depends on goroutine order or wall-clock speed?",
	"incomplete-assertion":      "Does the test assert the specific behavior its name or doc comment promises, even if it leaves other fields or return values unchecked?",
	"swallowed-error":           "Is ignoring or replacing this error deliberate and harmless: best-effort work, a value that cannot realistically fail, idiomatic cleanup, or a fallback the code or a comment documents?",
	"comment-contradicts-code":  "Is every claim in the comment that can be checked against the visible code consistent with that code?",
	"stub-body":                 "Is the minimal body intentional: a no-op or placeholder its name or comment describes, a generated or dependency-injection stub, or an implementation whose contract is to do nothing?",
	"iface-leaky":               "Is this interface a deliberately thin wrapper or test seam over a client or SDK, where exposing that client's types is the point?",
	"file-several-jobs":         "Do all the declarations serve one purpose, such as one command, one feature, or one test subject with its helpers?",
	"name-mismatch":             "Does the body do what its name says, read plainly?",
	"comment-excuse":            "Does the comment explain a deliberate choice or constraint rather than apologize for or defer a problem?",
	"comment-hedge":             "Does the comment state a known fact or documented limitation rather than leave doubt about whether the code is correct?",
	"comment-history":           "Does the comment mainly state current behavior or a binding constraint, with any history only as a passing reference?",
	"md-inanimate-actor":        "Is the subject of the marked verb a person, a role (a PM, the caller, the recipient, the on-call engineer), a team or organization, or an AI model, agent, or assistant (an LLM, Claude, the agent)?",
	"md-wordy-phrase":           "Would the one-word replacement change the meaning here (for example \"should be able to\", \"for the sole purpose of\", a negation, or a fixed user-story template), or is the phrase inside quoted text, a title, or code?",
	"md-announces":              "Is every sentence in the TARGET a fact, the document's opening statement of its own purpose or scope, a short lead-in to the list, table, or code block on the NEXT LINE, a question or prompt the reader is meant to answer, or a pointer to another page?",
	"md-restates":               "Does the TARGET state at least one fact, step, condition, destination, or example that neither the HEADING nor the PREVIOUS BLOCK states, or is it a lead-in to the block on the NEXT LINE?",
	"md-heading-promise":        "Does the section text say anything about the topic its heading names, even briefly, even to say it does not apply, has nothing to report, is deferred, or is covered by a link or a checked template box?",
	"md-two-names":              "Is each thing called by one name in the document's own prose, with any variants being ticket titles, file paths, code identifiers, or second names introduced as such where they first appear?",
	"md-hidden-verb":            "Is the noun phrase a standard term (for example \"access control\", \"code review\", \"go/no-go decision\"), a passive verb, or a lead-in to a list, rather than an action that one plain verb would say more directly?",
	"md-two-ideas":              "Does every clause in each sentence extend that sentence's main idea, for example by giving its reason, its result, its condition, or a detail of it, rather than start a separate one?",
	"md-prose-list":             "Are the inline items short words or names that read naturally in one sentence (such as \"Go, Kotlin, and TypeScript\"), or are the clauses one argument building to a conclusion, or is the sentence a fixed template?",
	"md-prose-table":            "Is the TARGET already a table, or do the things it describes have different attributes from each other?",
	"md-idiom":                  "Is every figurative phrase in the sentence a standard technical term (for example \"race condition\", \"smoke test\", \"blast radius\", \"fail open\", \"bit rot\"), a name the document gives its own project, process, or pattern (see DOCUMENT HEADINGS), or close enough to literal that a non-native reader would understand it?",
	"md-ambiguous-pronoun":      "Is there only one plausible referent for each pronoun, or would every plausible referent lead the reader to the same understanding and action?",
	"md-dropped-words":          "Does the TARGET have only one reasonable reading, so terse wording leaves the reader nothing to guess, or is it a label, table cell, short list item, code reference, or reference entry?",
	"md-topic-sentence":         "Does the paragraph have fewer than three sentences, or does its first sentence state or lead into the subject of the rest?",
	"md-one-topic":              "Do all the sentences serve the paragraph's main point?",
	"md-step-not-command":       "Is the list something other than steps the reader performs (for example principles, deliverables, test cases, or a description of what a system does), or is every step that does not start with a command verb a label for the code block or sub-steps under it, or the result of the step before it?",
	"md-list-not-parallel":      "Do the items match in grammar and kind well enough that a reader would not notice a mismatch, for example all following the same label-then-detail pattern?",
	"md-marketing":              "Is each strong adjective backed by a fact, number, or comparison in the same sentence or the one after it?",
	"md-blames-reader":          "Does the text describe the problem or the fix without saying the reader was at fault?",
	"md-howto-digression":       "Is the section something other than task instructions, or does every explanatory sentence tell the reader something they need to finish the task?",
	"md-no-purpose":             "Does the title or first paragraph tell the reader what the document is for?",
	"md-rfc-no-ask":             "Is the document something other than a proposal asking reviewers to approve a change, such as an idea, goal, or planning page, a README, guide, reference, notes, or a record of a decision already made?",
	"md-adr-many-decisions":     "Is the document something other than an architecture decision record, or does it record one decision with its parts?",
	"md-adr-no-cost":            "Is the document something other than an architecture decision record, or does it name at least one cost, downside, risk, or piece of follow-up work?",
	"md-one-sided-alternatives": "Does the document name no rejected alternatives, give at least one of them a real advantage, or is it something other than a decision document, such as a tracking table?",
}

// ruleFloors: tuned per rule on labeled findings, with 30% of files held out; unlisted rules need 0.75.
var ruleFloors = map[string]float64{
	"todo-no-owner": 0.9, "comment-history": 0.85, "comment-restates-name": 0.85, "flakey-test": 0.8, "comment-long": 0.8,
	"comment-tautology": 0.7, "comment-excuse": 0.7, "tautology-test": 0.7, "commented-code": 0.7, "comment-hedge": 0.6,
	"md-inanimate-actor":   0.6,
	"md-marketing":         0.85,
	"md-prose-list":        0.85,
	"md-list-not-parallel": 0.9,
	"md-idiom":             0.85,
	"md-step-not-command":  0.85,
	"md-no-purpose":        0.85,
	"md-ambiguous-pronoun": 0.85,
}

var ruleVetoCuts = map[string]float64{"flakey-test": 0.3, "comment-excuse": 0.7}

// defaultOff: rules under 50% precise after tuning, still available by name. On fresh docs most md rules here were
// 33-100% false; md-idiom tops out near 60% though judges agree, and md-noun-as-verb is too rare to measure.
var defaultOff = map[string]bool{
	"assert-self": true, "comment-contradicts-code": true, "file-several-jobs": true, "iface-god-method": true,
	"iface-leaky": true, "incomplete-assertion": true, "name-mismatch": true, "stub-body": true,
	"swallowed-error": true, "test-except-pass": true, "wrong-assertion": true,
	"comment-hedge": true, "comment-excuse": true, "comment-long": true, "commented-code": true, "comment-history": true,
	"md-marketing": true, "md-heading-promise": true, "md-prose-table": true, "md-restates": true,
	"md-ambiguous-pronoun": true, "md-step-not-command": true, "md-howto-digression": true, "md-blames-reader": true,
	"md-idiom": true, "md-noun-as-verb": true, "md-announces": true, "md-dropped-words": true, "md-hidden-verb": true,
	"md-one-sided-alternatives": true, "md-one-topic": true, "md-rfc-no-ask": true, "md-two-ideas": true, "md-two-names": true,
}

func ruleFloor(id string) float64 {
	if f, ok := ruleFloors[id]; ok {
		return f
	}
	return 0.75
}

func vetoID(id string) string { return id + "~veto" }

// withVetoes: the job's rules plus each one's exception question.
func withVetoes(rules []Rule) []Rule {
	out := rules
	for _, r := range rules {
		if q, ok := ruleVetoes[r.ID]; ok {
			out = append(out[:len(out):len(out)], Rule{ID: vetoID(r.ID), Kinds: r.Kinds, Scope: "question", Rubric: q})
		}
	}
	return out
}

// attachVetoes keeps only the job's rules, recording each exception's probability on its verdict.
func attachVetoes(rules []Rule, v map[string]Verdict) map[string]Verdict {
	out := make(map[string]Verdict, len(rules))
	for _, r := range rules {
		vv := v[r.ID]
		if veto, ok := v[vetoID(r.ID)]; ok {
			p := veto.Confidence
			vv.Veto = &p
		}
		out[r.ID] = vv
	}
	return out
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
