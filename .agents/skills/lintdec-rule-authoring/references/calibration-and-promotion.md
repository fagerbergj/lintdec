# Calibration Lab, Dogfooding, and 4-Tier Promotion Process

## 1. Blind Synthetic Recall Lab (Step 5)

Before dogfooding a new rule on live repos, prove it works on a small, blind corpus.

### Fixture Construction Rules
- Place fixtures in an isolated directory (outside git worktrees, which `lintdec` skips).
- **Zero hint leakage:** Never put `// PLANTED <rule-id>` comments inside fixtures, and never put diagnostic labels like `(ambiguous)` or `(bad)` in Markdown headings or test names.
- **Pair every loud plant with at least one quiet twin:**
  - *Loud plant:* textbook violation that MUST fire above floor and below veto cut (`< 0.50`).
  - *Quiet twin:* realistic near-miss that exercises the pre-filter and exemption (e.g., `TODO(PROJ-456)` for `todo-no-owner`, `pytest.raises` for panic/exception handling, clean ADR with costs and fair pros/cons for ADR rules).
- **Separate Go comments with statements:** Go's `go/ast` merges consecutive comment lines into a single `Comment` unit.
- **Keep ground truth external:** Store expected loud and quiet targets in a separate `manifest.json` outside the linted `corpus/` directory.

### Subsumption Check
Always run the lab with the new rule **plus every existing default-on rule that shares its `Unit.Kind`**. If an existing default-on rule already catches 100% of the new rule's loud plants cleanly (as `tautology-test` did for `assert-self` and `test-except-pass`), do not add the redundant rule.

### Lab Run Commands
1. Verify unit splitting & pre-filters first (free, zero LLM calls):
   ```bash
   lintdec --rules <new-rule>,<sibling-defaults> --dump-units ./corpus
   ```
2. Check that no `quack` review session is currently using Clef (`-np 1`):
   ```bash
   quack --url http://100.64.0.6:18081 sessions --state running
   ```
3. Run Clef cold (`--no-cache --calibrate --threshold 0.50`) so both primary confidence and veto scores are recorded:
   ```bash
   lintdec --no-cache --protocol systemone \
     --base-url http://100.64.0.6:11436/upstream/clef \
     --model clef --workers 1 \
     --rules <new-rule>,<sibling-defaults> \
     --calibrate --threshold 0.50 --out jsonl --quiet ./corpus > /tmp/lab.jsonl
   ```
4. Score with `scripts/score-lab.py`:
   ```bash
   python3 .agents/skills/lintdec-rule-authoring/scripts/score-lab.py /tmp/lab.jsonl ./manifest.json
   ```

### Lab Exit Bar
- **Loud recall:** ≥ 90% on textbook plants (100% on small 2–4 plant sets), with comfortable floor margin and veto `< 0.35` (watch out for vetoes hovering near `0.45–0.49`).
- **Quiet false positives:** 0 reportable hits on quiet twins.
- **Zero subsumption:** At least one distinct defect class missed by existing default-on rules.

## 2. Multi-Repo Dogfooding (Step 6)

Lab success proves recall, not real-world precision or cost. Dogfood the opt-in rule across **at least two contrasting codebases**:

- **A clean, lint-enforced repo** (e.g., `quack` or `lintdec`) — exposes false-positive edge cases on mature code (like live compatibility comments or domain terminology).
- **One or more diverse active repos** (e.g., `games`, `home-server`, `dotagents`, `NightsOut`) — exposes real-world recall across languages and doc styles.

Measure four numbers per rule:
1. **Eligible units** after pre-filters (Clef marginal calls).
2. **Marginal Jev batches** (`+0` if its `Unit.Kind` already has a default-on rule).
3. **Reportable findings & verified precision** (read the surrounding source of every hit — never assume a hit is true).
4. **Protocol split:** Does the rule stay clean on chat Jev (no vetoes) as well as Clef (with vetoes)? Veto-dependent rules stay `defaultOff` unless their floor alone holds precision.

## 3. The 4-Tier Promotion & Pruning Matrix (Step 7)

| Tier | Empirical Signal | Action |
|---|---|---|
| **Tier 1 — Golden** | High verified precision (≥ 85–90% on Clef), real actionable catches, low marginal cost (pre-filtered or piggybacks on existing kind). | **Promote to default-on** (remove from `defaultOff`). |
| **Tier 2 — Opt-In** | Real catches, but arguable maintainability value, high batch cost on unfiltered kinds, or high false-positive rate without System One vetoes. | **Keep in `defaultOff`**; tighten veto/floor if noisy. |
| **Tier 3 — Zero Findings** | Zero above-floor hits in dogfooding (insufficient data). | **Keep in `defaultOff`**; re-check recall in blind synthetic lab before deciding keep vs. delete. |
| **Tier 4 — Noisy / Dead / Subsumed** | High false positives even on Clef, structurally uncheckable from unit context, 0% recall in lab, or 100% subsumed by a default rule. | **Delete** from `rules.go`, `veto.go`, `filters.go`, and record in `tuning-fps.md`. |

## 4. Recording Evidence in `tuning-fps.md`

Every rule addition, promotion, demotion, or deletion must append a brief dated note to `tuning-fps.md` capturing:
- Corpus tested (repos, file/unit counts, model/protocol).
- Verified true-positive / false-positive counts or lab scores.
- Pre-filter decision savings (before/after `--dump-units` counts).
