# Rule Admission, Pre-Filters, Rubrics, and Isolated Vetoes

## 1. The Five Admission Gates

A candidate rule enters `lintdec` only if it passes **all five** gates:

| Gate | Requirement | Fail Example (Reject) |
|---|---|---|
| **1. Semantic Judgment** | Cannot be decided accurately by AST, regex, type-checking, import graph, or advisory lookup alone. | Unused vars (`tsc`), import cycles (`sloplint`/`madge`), cyclomatic complexity (`sloplint`), duplicate clones (`jscpd`), CVEs (`osv-scanner`), raw LOC/param counts. |
| **2. Local Visibility** | Provable strictly from `TARGET` + `CONTEXT` of one unit. Code not shown is never evidence. | `wrong-assertion` (SUT body absent from `TestFunction`), `comment-stale` (requires git history/wall clock), dead exports (requires repo-wide graph). |
| **3. Primary Source** | Grounded in an authoritative style guide, engineering handbook, or peer-reviewed study with a followable URL (`://`). | Ad-hoc house preferences with no cited rationale (`TestEveryRuleHasSource` enforces URL presence). |
| **4. Actionable Fix** | Names one concrete, low-ambiguity edit the owning author can make in the current diff (Google Tricorder / Yelp EE Metrics standard). | Churn density, bus factor, PR size, or vague "hard to read" aesthetics. |
| **5. Non-Redundant** | Catches defects that existing default-on rules miss in a blind lab. | `assert-self` and `test-except-pass` (100% subsumed by `tautology-test`), `comment-restates-name` (folded into `comment-tautology`). |

### Deterministic vs. Semantic Split

When mining research (e.g., maintainability or sloppiness catalogues), classify every predicate before touching `lintdec`:

- **Class D (Deterministic) → `sloplint` / language linter / scorecard:** AST structure, import boundaries, unused symbols, empty blocks, self-compare, secret regexes, CVE lookups, branch protection APIs.
- **Class H (Threshold / Metric) → `sloplint` / scorecard (raw value + trend):** Cyclomatic/cognitive complexity, duplication ratio, function/file length, dependency lag, test duration.
- **Class S (Semantic / Intent) → `lintdec` candidate:** Contradiction between comment and visible code, mock choreography hiding production logic, boolean flag splitting unrelated behaviors, unactionable error docs, prose lists, or skipped tests lacking real capability guards.

## 2. Unit Kind & Pre-Filter Design

Prefer existing `Unit.Kind` values unless the rule's target shape would inherit mismatched rules:

- **Reuse existing kind** when all rules on that kind make sense on the unit (`Comment`, `TestFunction`, `Function`, `Interface`, `File`, `Paragraph`, `Bullet`, `Section`, `Document`).
- **Add a dedicated kind** when reusing a kind would trigger irrelevant rules (e.g., `Script` for anonymous top-level shell bodies so `swallowed-error` runs without `name-mismatch` or `stub-body`; `Directive` for linter suppressions; `LintConfig` for linter config files).

### Pre-Filter Rules (`filters.go` & `candidates.go`)

Always look for a cheap trigger-preserving pre-filter **before** paying for LLM calls:

1. **Hook into `ruleFilters` in `filters.go`** (consulted by `buildJobs` via `ruleEligible`). This preserves Jev batching and cache keys without modifying splitters.
2. **Use RE2-safe regexes only** (Go `regexp` panics on `(?!...)` / `(?<=...)`).
   - Good token boundary avoiding hyphenated IDs: `(?i)(?:^|[^-\w])(?:TODO|FIXME|HACK|XXX)(?:[^-\w]|$)`
   - Test punctuation endings (`.`, `!`, `,`, `)`) AND hyphenated negative cases (`todo-no-owner`, `no-todo`, `todolist`).
3. **Include escape hatches for formatting variance:** Line-count proxies should include a word-count escape hatch for unwrapped text (e.g., `comment-long`: `lines >= 3 || words >= 50`).
4. **Do NOT pre-filter when cross-language idioms defeat lexical cues:** For example, `swallowed-error` stays unfiltered because Go (`os.Remove(p)`), Rust (`let _ =`), and JS (`void fn()`) drop errors without `catch`/`except`.
5. **Pin `buildJobs` wiring in `filters_test.go`:** Always test both `ruleEligible` directly and `buildJobs` end-to-end so deleting the call site breaks tests.

## 3. Authoring Rubrics (`rules.go`)

- **Judgment, no math:** Never put numeric thresholds ("more than 5 parameters", "over 80 words") in the rubric text; counting degrades System One judgment (`tuning-fps.md`, 2026-11-21). Put structural floors in `filters.go` instead.
- **Concrete failure modes, 1–3 sentences:** State what constitutes a violation using visible evidence in `TARGET` or `CONTEXT`.
- **Single clear `ruleLevel`:**
  - `error`: defect likely to hide broken code or break tests (`flakey-test`, `tautology-test`, `swallowed-error`).
  - `warning`: misleading or broken contract/doc (`comment-contradicts-code`, `iface-leaky`, `md-no-purpose`).
  - `info`: maintainability or clarity smell (`comment-tautology`, `comment-long`, `md-prose-list`).
- **Source format:** `<Author/Guide>, <Section>: "<short quote or gloss>" <URL>`. Doc rules prefix their catalogue ID (e.g., `C-10 · ...`).

## 4. Authoring Isolated System One Vetoes (`veto.go`)

System One runs (`systemone-1q-iso`) ask `[rule]` alone first, and only ask `[rule~veto]` in a **second isolated request** when primary confidence `>= min(0.50, floor)`.

Rules for writing a reliable isolated veto:

1. **Positive framing:** Ask whether the **legitimate exemption** holds (`yes` → suppress finding when `veto >= 0.50`). Never double-negate.
2. **100% self-contained (zero dangling anaphora):** The veto prompt does NOT see the primary rubric.
   - Bad: `"Is the noun phrase a standard term...?"` (when asked alone, `"the noun phrase"` had no referent and vetoed a true positive at `0.908`).
   - Good: `"Is every weak-verb-plus-noun phrase in TARGET (such as \"performs an evaluation of\") actually a standard term (for example \"access control\", \"code review\")...?"` (`0.908 → 0.077`).
3. **Name exact exemptions observed in real repos:** E.g., `comment-history` explicitly exempts live legacy data/schema compatibility notes and in-test scenario timelines (`v1/v2` fixture steps).
4. **Skip a veto if the rubric has no common false-positive exemption:** Rules like `todo-no-owner`, `tautology-test`, and `comment-tautology` run at 100% precision on Clef without a veto.
