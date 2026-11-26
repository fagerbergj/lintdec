---
name: lintdec-rule-authoring
description: |
  End-to-end process for proposing, authoring, pre-filtering, lab-testing, dogfooding, and promoting or pruning lintdec decision-model rules.
  Use whenever adding a new lintdec rule, mining research sources for rule candidates, writing a rule rubric or isolated System One veto,
  designing a deterministic pre-filter in filters.go, running a blind synthetic recall lab on Clef, or auditing rules for promotion or deletion.
license: MIT
metadata:
  author: fagerbergj
  version: "1.0"
---

# Lintdec Rule Authoring & Calibration

## Overview

This skill defines the repeatable lifecycle for taking a `lintdec` rule from a primary source to default-on (or rejection): admission check, unit/pre-filter design, rubric and isolated-veto authoring, offline wiring tests, blind synthetic recall lab on Clef, multi-repo dogfooding, and evidence logging in `tuning-fps.md`. It stops at shipping the verified rule change in `lintdec` — it does not implement deterministic linters (`sloplint`, `eslint`, `knip`) or repository-wide scorecard pipelines.

## When to Use

- Adding a new code, test, shell, or Markdown rule to `lintdec`.
- Mining engineering research or style guides for candidate `lintdec` rules.
- Writing or revising a rule rubric (`rules.go`), isolated System One veto (`veto.go`), or deterministic pre-filter (`filters.go` / `candidates.go`).
- Building a synthetic recall lab or running a multi-repo audit to promote, tune, or delete rules.

## When NOT to Use

- Do not use for checks that are 100% deterministic via AST, regex, type-checking, import graphs, or advisory DBs (unused vars, import cycles, cyclomatic complexity, duplication, secret scanning, CVEs, formatting). Put those in `sloplint` or a deterministic linter instead.
- Do not use for metrics that require unshown call sites, full git history, CI telemetry, or runtime execution (mutation scores, flakiness replay, staleness by date, bus factor, PR latency). Those belong in a repo scorecard.
- Do not use for fixing findings in target repositories; this skill modifies `lintdec` itself.

## Step-by-Step Procedure

- [ ] **Step 1 — Admission Check (5 Gates):** Verify the candidate passes all five admission gates in `references/admission-and-rubric-guide.md`: (1) requires semantic judgment, (2) decidable from `TARGET` + `CONTEXT` alone, (3) backed by a primary source URL, (4) gives the author one concrete fix in the diff, and (5) is not subsumed by an existing default-on rule. Reject early if any gate fails.
- [ ] **Step 2 — Pick Unit Kind & Pre-Filter First:** Choose the narrowest existing `Unit.Kind` (`Comment`, `Directive`, `LintConfig`, `TestFunction`, `Function`, `Script`, `Interface`, `File`, `Paragraph`, `Bullet`, `Step`, `Steps`, `MixedList`, `Section`, `Document`, `ActorVerb`, `NounVerb`, `WordyPhrase`), or add a dedicated kind if existing kinds bind unrelated rules (as `Step`, `Script`, `Directive`, and `LintConfig` did). Before writing any prompt, design a trigger-preserving deterministic pre-filter in `filters.go` (or candidate extractor in `candidates.go`) so irrelevant units cost zero model calls.
- [ ] **Step 3 — Draft Rubric & Isolated Veto:** Write the `Rule` in `rules.go` using judgment-only prose (no line/word/parameter counts in prompts) with a followable `https://` or `http://` source URL and fixed `ruleLevel`. If the rule has common legitimate exceptions, write a **self-contained, positively framed** veto question in `veto.go` (`ruleVetoes`). Always start new rules in `defaultOff: true` until Step 6 passes.
- [ ] **Step 4 — Wire & Run Offline Verification:** Register the rule in `rules.go`, `veto.go`, and (if filtered) `filters.go`. Add unit tests covering pre-filter edge cases (punctuation, hyphenated rule IDs, unwrapped text) and `buildJobs` wiring in `filters_test.go`. Run:
  ```bash
  gofmt -l . && go vet ./... && go test ./...
  ```
- [ ] **Step 5 — Run Blind Synthetic Recall Lab on Clef:** Create blind fixtures (neutral filenames and headings, zero `PLANTED` hint comments) with both **loud plants** (must fire) and **quiet twins** (must stay silent). Run Clef (the local single-slot System One provider, configured as `clef`) with `--no-cache --calibrate --threshold 0.50 --out jsonl` over the new rule **plus all default-on rules for that unit kind** to test both recall/precision and subsumption. Score results with `scripts/score-lab.py`. A rule that misses textbook plants or is 100% caught by an existing default rule does not advance.
- [ ] **Step 6 — Multi-Repo Dogfood & 4-Tier Audit:** Run the opt-in rule across at least two diverse repositories (not only a single `sloplint`-clean repo) on Clef (or Jev + Clef spot check). Classify every hit as true positive or false positive and compute marginal decisions and finding rate per `references/calibration-and-promotion.md`.
- [ ] **Step 7 — Decide Default Status, Record Evidence, and Gate Diff:**
  - Promote out of `defaultOff` only if it lands in Tier 1 (high precision on Clef, clear maintainability value, acceptable marginal cost).
  - Keep in `defaultOff` if Tier 2 (real hits, high cost or narrow applicability).
  - Keep in `defaultOff` and re-check recall in the blind lab if Tier 3 (zero above-floor hits — insufficient data).
  - Delete if Tier 4 (high false positives, missing-context flaw, or subsumed).
  - Record measured counts, lab outcomes, and decisions in `tuning-fps.md`.
  - After committing, run `go run ./tools/sloplint diff origin/main` before pushing.

## Gotchas

- **Go `regexp` is RE2 (no lookaheads):** Negative lookaheads like `(?!...)` panic at init. Use explicit boundary classes like `(?:^|[^-\w])TAG(?:[^-\w]|$)` so hyphenated IDs (`todo-no-owner`, `no-todo`) stay excluded while punctuation-terminated tags (`TODO.`, `FIXME!`, `(xxx)`) match.
- **System One vetoes are lazy and isolated (`systemone-1q-iso`):** The veto question is sent in a **separate** `/v1/systemone` request without the primary rubric. Never use dangling references like `"the noun phrase"` or `"this error"` — always name `TARGET` and the exact construct so the question makes sense standalone.
- **Lab leakage invalidates recall numbers:** Never put `// PLANTED <rule-id>` comments or diagnostic parentheticals like `## Forwarding (ambiguous)` in lab fixtures — `HEADING:` and surrounding comments are passed to the model in `CONTEXT`.
- **Consecutive Go comments merge into one `Comment` unit:** Separate independent comment test cases with statements in Go lab fixtures, and verify unit boundaries with `lintdec --rules <id> --dump-units <dir>` before calling Clef.
- **Never run two single-slot System One jobs concurrently:** If your local System One server is single-slot (`-np 1`) or shared with review automation, check for active jobs before starting a sweep and keep `--workers 1`.
- **Bump `cacheTemp()` on request-shape changes:** Editing a rubric or veto automatically invalidates that rule via `ruleVersions`, but changing how `/v1/systemone` calls are structured requires bumping `cacheTemp()` in `decide.go`.
- **Single-repo blind spots cut both ways:** A `sloplint`-clean repo hides real recall on opt-in rules, while a mature repo exposes false positives (such as live backward-compatibility comments on `comment-history`) that small repos miss.
- **Invariants enforced by `go test ./...`:** `TestEveryRuleHasSource` requires `://` in `Rule.Source`; `TestEveryFilterMapsToKnownRule` fails on stale rule IDs in `ruleFilters`, `ruleVetoes`, `ruleFloors`, or `defaultOff`.

## Validation Loop

Run this loop after every rule edit until every check passes cleanly:

1. Build & unit-test: `gofmt -l . && go vet ./... && go test ./...`
2. Verify unit boundaries & pre-filter savings: `go run . --rules <id> --dump-units <lab-or-repo>`
3. Run blind recall lab on Clef (`--no-cache --calibrate --threshold 0.50 --out jsonl`) and score with `python3 .agents/skills/lintdec-rule-authoring/scripts/score-lab.py <findings.jsonl> <manifest.json>`
4. Commit and run the diff quality gate: `go run ./tools/sloplint diff origin/main`

## Resources

- Read `references/admission-and-rubric-guide.md` during **Steps 1–3** to apply the 5-gate admission rubric, check the deterministic-vs-semantic boundary, and author rubrics, RE2 pre-filters, and isolated vetoes.
- Read `references/calibration-and-promotion.md` during **Steps 5–7** to structure blind synthetic labs, run Clef/Jev dogfood sweeps, and apply the 4-tier promotion/deletion matrix.
- Read `references/mined-rule-backlog.md` when **choosing the next rule to build** — contains vetted candidates and explicit rejections mined from published maintainability, testability, static-analysis, and documentation primary sources and filtered through the 5 admission gates.
- Run `scripts/score-lab.py` during **Step 5** to score a `lintdec --calibrate --threshold 0.50 --out jsonl` lab run against a blind JSON manifest for recall, quiet false positives, veto margins, and subsumption.
