# FP ledger for rubric tuning

Collected 2026-02-19 from the full active-repo lint (quack 83, home-server 16, dotagents 28, games 2 findings; NightsOut clean). Each entry: what fired, why it is a false positive or weak, the rubric gap, and the proposed patch. Lab plants proposed per class so the calibration set covers the fix.

## md-table-dense

### FP-1: working-document tables (the big one, ~90 findings)

Evidence:
- quack `.quack/v1-design/audits/*` (60 of the 60 quack hits are in v1-design): audit tables with columns like Agent | Evidence | What it shows, SRS requirement tables, RFC extension tables
- home-server ops docs (11): hardware inventory ("Board cell packs multiple distinct specs"), networking setup, LLM build notes
- dotagents skills references (19): `http-methods.md` (PATCH semantics cell), `richardson-model.md`, `spec-requirements.md`, mermaid shape tables, `api-versioning.md`, `severity.md`

Why FP: these are reference/working documents where the cell IS the content. The semantics of PATCH cannot be a short phrase; an audit's "What it shows" column exists to carry the reasoning. The rule has no medium or audience test, so it floods precisely the docs whose tables are load-bearing.

Rubric gap: md-jargon carries an audience clause ("operational and personal notes … legitimately use their domain's vocabulary"); md-table-dense has none.

Proposed patch (append to the rubric):
> Judge the file's role from its path and headings. Reference and working documents — API and protocol references, RFCs, SRS and audit records, design docs, hardware or network inventories, build notes — legitimately pack structured facts into cells, and their tables are the format, not a defect. Do not flag them. The target is reader-facing prose documents (public READMEs, tutorials, changelogs, user guides) where a cell carries a multi-sentence essay the reader must parse inline.

### FP-2: short two-clause cells

Evidence:
- games `README.md` protocol table: three rewrite rounds were needed before the table went clean. The last trigger was a ~15-word cell: "Join a room; a token from an earlier joined re-claims that seat and host role". Before that, an 8-item parallel enumeration with phase tags (`hit` … on your turn, `insurance` … in the insurance phase) was flagged despite the rubric already saying "do not flag cells whose length comes from many short parallel entries".

Why FP: the rubric's "multi-sentence content" trigger has no length floor. Two short clauses separated by a semicolon is not an essay, and a parallel enumeration with conditions is the reference form.

Proposed patch:
> A cell under ~25 words is not dense unless it packs three or more distinct facts. Parallel enumerations (a list of values, actions or options, each with at most one short qualifier) are the reference form of such tables and are never dense.

Lab plants to add (must stay quiet):
- a protocol reference table with semicolon two-clause cells and one parallel enumeration
- an audit table (Agent | Evidence | What it shows) with 2-3 sentence "What it shows" cells
- a hardware inventory table with multi-spec cells
Plants that must still fire:
- a public README table with a 4-sentence essay cell (the existing planted.md table)

## md-jargon

### FP-3: terms defined later in the same file

Evidence: quack `.quack/v1-design/rfc/extensions/slots.md:8` — the RFC Summary uses lease ledger / GenerationTask / fake clock; all are defined in the Design section of the same file. The rubric says "do not flag terms defined or referenced within the file", but the model only looks at the flagged paragraph.

Proposed patch:
> A term that is defined, introduced or explained anywhere in the same file — including later sections — is not jargon, even when the paragraph using it comes before the definition.

Lab plant: an RFC-shaped doc whose Summary uses a term that its Design section defines (must stay quiet), alongside the existing broad-audience plant.

### FP-4: a product's own component nouns in its own README

Evidence: quack `README.md:3` — "A distributor runs one session process per conversation, each driving an agent bundle on pi-durable". Distributor/agent bundle are the product's names for its parts, explained in the docs the README links to.

Proposed patch:
> A product or project's own names for its components are not jargon in that project's own documentation, even when the names are internal.

## comment-tautology

### FP-5: restates the name but adds a mechanism

Evidence: dotagents lab `bigfile.go:29` — `// Put stores a value, evicting the oldest slot.` on method `Put`. The first clause restates the name, but the comment adds the eviction behavior, which the name does not carry. Firing tautology here overlaps with comment-restates-name (the more precise rule) and loses the "adds information" test.

Proposed patch:
> If any clause of the comment states a mechanism, constraint or behavior the name does not state, the comment is not tautological, even if another clause restates the name. (Update 2026-11-26: `comment-restates-name` was later removed and pure name restatement folded into `comment-tautology` with this guard.)

## md-para-long (weak, not clear FP — review list)

- home-server ops READMEs (5 hits): operational setup notes where dense paragraphs are the habit. The rubric already requires "mixes multiple distinct points", and the messages cite that, so these are probably right — but the rule has no audience clause like md-jargon's. Keep for now; revisit if ops notes keep flooding.

## Not FPs (verified true, for the record)

- games `README.md` hard-wraps at 103/125/136/171: genuine fixed-width wrapping, fixed and verified clean.
- quack `docs/README.md` 7 para-long: genuine 200-word CLI walls.
- quack v1-design para-long (~27): genuine multi-point walls even in design docs.

## Secondary tuning note

Confidence is still ~0.95 for nearly everything (0.85-0.99 band, no spread). It works as a gate; it is useless as a sort key. If we ever sort by it in CI triage, add explicit spread anchors to the system prompt ("0.6 = pattern present but the unit could be intentional; 0.85+ only when a human would agree without rereading").

## Status of proposed rubric changes — APPLIED 2026-02-19

All five rubric patches are in rules.go. New quiet plants added: `references.md` (Q1-Q3 protocol/audit/inventory tables), README sections (Q4 terms-defined-later, Q5 project's own names), bigfile.go:29 (Q6, restated as a constraint the code does not state). Calibration: two fresh runs both 22/22 loud + 6/6 quiet. One bad plant caught by calibration: the original Q6 comment (`evicting the oldest slot`) was rightly flagged — the ring write is visible eviction — so the plant now carries a constraint the code does not state.
Residual: 4 borderline units (lab hard-wrapped paragraphs, the ParseFlags doc comment, one decision-record) flip between fresh runs at temperature 0 — GPU-batching nondeterminism confined to the borderline. 22/22 and the quiet plants are stable across runs.
Structural guards (in lint.go, enforced after the model verdict, until md-hard-wrap was removed; see below): a single-line unit can never be md-hard-wrap, and md-hard-wrap needs at least two mid-sentence line breaks — a definition of where the line breaks fall, not a size judgment; and findings anchor to the unit's real span, because the model's line numbers drift once the file is edited around the unit.

Judgment-only correction (2026-11-21): the numeric floors were removed — the 80-word code floor for md-para-long and the "N sentences/words/lines/parameters" anchors in the md-para-long, comment-long, complex-method, iface-god-method and md-table-dense rubrics. Standing rule is judgment, no math, and thresholds train the model to count instead of read. The md-para-long rubric now says "separate topic, where a reader must reorient" — a sentence or clause that develops the same point is one point. Re-calibrated 26/26 twice, quiet plants silent. Residual: a small class of dense single-topic paragraphs (a ~120-word one-idea paragraph) still flips between runs; that is the model's honest borderline, left to judgment rather than re-floored.

## Removed rules

Eight rules on the published `main` (`626ff5d`) are removed in the change that adds the sourced documentation rules:

- `complex-method` and `md-para-long`, after a self-lint run of lintdec at `78ebd21`. `complex-method` produced 14 of the run's 34 false positives (7 valid of 21), mostly long functions that still do one job; deterministic cyclomatic complexity (sloplint) covers the measurable part. `md-para-long` was 3 valid of 6 and kept flipping on dense single-topic paragraphs.
- `comment-stale` and `comment-decision-record`, 0 of 17 valid across two self-lint runs: neither staleness nor decision-worthiness is visible in the comment's text. `comment-contradicts-code` (a contradiction visible in the shown code) and `comment-history` (narrated history) replace them.
- `big-file`, replaced by `file-several-jobs`, which judges unrelated topics rather than size.
- `md-hard-wrap`, a line-length pattern a deterministic linter checks better; and `md-table-dense` and `md-jargon`, which stayed far above 10% false. The sourced documentation rules replace them, each citing a published style rule (`lintdec --list-rules` shows the source).

Seven more rules (`47 -> 40`) are removed after a 5-repo all-47-rule audit (`lintdec`, `NightsOut`, `games`, `home-server`, `dotagents`; 500 files, 11,240 batches, 49,072 Jev decisions + full Clef runs on `quack` and `lintdec`, 2026-11-26):

- `md-idiom` (376 findings, ~100% false positives on normal technical prose; `Sentence` unit generation removed with it), `md-dropped-words` (74/74 false positives on terse CLI/table prose), `md-announces` (37/37 false positives on normal lead-in sentences), `md-two-ideas` (25/25 false positives on technical trade-off sentences), `wrong-assertion` (4/4 false positives even on Clef because the SUT body is not in the `TestFunction` unit), `comment-restates-name` (1/11 valid, folded into `comment-tautology` with FP-5's mechanism guard), and `commented-code` (overlapping noise on commented config/examples; deterministic checks fit better).
- Promoted to default-on: `comment-history`, `iface-leaky`, `comment-contradicts-code`, `comment-hedge` (floor raised `0.60 -> 0.75`), and `comment-long` (paired with a `>= 3` lines or `>= 50` words pre-filter and System One veto). Moved to `defaultOff` pending synthetic recall lab: `md-topic-sentence`, `md-adr-many-decisions`, `md-adr-no-cost`.
- Deterministic pre-filters added in `filters.go` (`ruleEligible`) cut all-rules decisions by 22.9%–43.5% across the audited repos (`lintdec` 5,941 → 4,245; `NightsOut` 2,427 → 1,872; `games` 10,842 → 7,556; `home-server` 12,943 → 7,319).

Three more rules (`40 -> 37`) are removed after a 16-rule synthetic recall lab and full 671-file `quack` Clef run (2026-11-26):

- `assert-self` and `test-except-pass`, both 100% subsumed by default-on `tautology-test` (`0.874–0.983` on every `assert-self` and `test-except-pass` plant, 0 quiet false positives).
- `md-ambiguous-pronoun`, 0% recall even on a textbook 4×-`"it"` sentence (`< 0.50`) and 0 above-floor findings across 500 real files.
- `comment-history` returned to `defaultOff` with a tighter veto exempting live legacy-format compatibility notes and in-test scenario timelines after producing 32/37 findings on `quack` (9 false positives + 23 regression-test provenance notes).

## Added maintainability & testability rules (2026-11-26)

Five pre-filtered rules (`37 -> 42`) added after blind synthetic recall lab (`18s`, Clef `systemone-1q-iso`) and 4-repo Clef dogfood (`lintdec`, `quack`, `games`, `home-server`; 977 files, 15,541 units). Across the 4 repos at `2d21140`, deterministic pre-filters and the `Directive` kind reduce combined candidate decisions from `8,626` to `87` (`99.0%` filtered): `test-shared-state` `2,721 → 37` (`98.6%`), `test-ignored-no-reason` `2,721 → 5` (`99.8%`), `test-over-mocked` `2,721 → 21` (`99.2%`), `md-no-remediation` `463 → 10` (`97.8%`), and `suppression-no-reason` `14` `Directive` units (vs `5,492` surviving `Comment`+`Directive` units). `filterComments` only re-kinds pure suppression blocks that `structuralComment` previously dropped, so mixed Go prose+directive blocks stay `Comment` units with zero loss of prose-rule coverage (with the deliberate trade-off that a bare suppression embedded inside a multi-line prose comment is not routed to `suppression-no-reason`).

- **Default-on (Tier 1):**
  - `test-shared-state`: 2/2 lab recall (`0.975–0.991`, veto `0.009–0.021`, 0/1 quiet FPs; Go `/tmp/` plant `L46` was not caught by `tautology-test` at `0.595 < 0.70`), 3/3 real true positives on un-restored `process.env.QUACK_EXA_API_KEY` leaks in `quack`, 3 isolated tests cleanly vetoed, 0 FPs.
  - `suppression-no-reason` (`Directive` kind): 1/2 lab recall (`0.953`, veto `0.018` on tautological `@ts-ignore fix type error`, 0/2 quiet FPs; bare line-2 `eslint-disable` was missed in the clustered fixture where an explained disable sat 4 lines below in CONTEXT), 9/9 real true positives on bare `eslint-disable`, `shellcheck disable`, and `# noqa` across `games` and `home-server`, 0 FPs.
- **Opt-in (`defaultOff`, Tier 3 — 100% lab recall, 0 real FPs, 0 real hits across the 4 repos):**
  - `test-ignored-no-reason`: 3/3 lab recall (`0.974–0.990`, veto `0.011–0.021`, 0/2 quiet FPs; non-subsumption verified against `tautology-test` on `it.skip(...)` in `billing.test.ts`).
  - `test-over-mocked`: 2/2 lab recall (`0.878–0.969`, veto `0.024–0.231`, 0/1 quiet FPs; non-subsumption verified against `tautology-test` on `billing.test.ts:15`; 1 legitimate UI boundary mock cleanly vetoed at `0.962` in `quack`).
  - `md-no-remediation`: 2/2 lab recall (`0.889–0.941`, veto `0.052–0.072`, 0/2 quiet FPs), plus follow-up Clef check confirming fenced-command quiet twins (where `proseLines` blanks the body to `[sh code block]`) stay silent or veto at `0.884`, while level-5 `##### Known issues` loud plants fire at `0.872` (veto `0.074`).
- **Rejected in dogfood (Tier 4):** `flag-argument` passed the synthetic lab (2/2, `0.973–0.983`, veto `0.035–0.068`, 0/2 quiet FPs) but produced 66 findings across 4 repos, mostly normal single-boolean domain parameters (`isSoft`, `isSplitHand`, `dealerHitsSoft17`, `allowPort`, `reveal`), so it was dropped before merge.

## Added meta-linting, hand-rolled stdlib & retry rules (2026-11-26)

Three pre-filtered opt-in rules (`42 -> 45`) added after blind synthetic recall lab (Clef `systemone-1q-iso`, 18s) and 4-repo Clef dogfood (`lintdec`, `quack`, `games`, `home-server`; 980 files, 15,551 units, 137 decisions across the 3 shipped rules, 2m06s total, 0 errors), plus a `Directive` context fix (`precedingRationale` in `split.go`, `commentContext` in `treeSplit.go`/`split.go`) so preceding rationale comments above ShellCheck/Go/Python suppressions travel in `TARGET` (verified live on Clef: documented `# shellcheck disable=SC2206` drops to silent `< 0.50`, while bare `# shellcheck` fires at `0.960` and non-rationale `# TODO` above `# shellcheck` still fires at `0.953`):

- **Opt-in (`defaultOff`, Tier 3 — 100% lab recall, 0 quiet FPs, 0 real FPs across 980 files):**
  - `lint-config-weakened` (`LintConfig` kind, `warning`, 2 decisions across 4 repos): 2/2 lab recall (`0.977–0.979`, veto `0.012–0.016`, 0/2 quiet FPs on clean `eslint.config.mjs` and `lintdec.yaml`), 0 FPs on live repo configs (`quack/lintdec.yaml`, `games/eslint.config.mjs`).
  - `hand-rolled-stdlib` (`Function`, `warning`, 66 decisions across 4 repos): 2/2 lab recall (`0.966–0.988`, veto `0.048–0.100`, 0/2 quiet FPs on stdlib delegation and documented hot-path minimal subset), 0 FPs across 66 real candidate functions.
  - `unbounded-retry` (`Function`, `Script`, `error`, 69 decisions across 4 repos): 2/2 lab recall (`0.991–0.992`, veto `0.026–0.036`, 0/2 quiet FPs); one initial real-world pagination `while True:` hit (`home-server/torrent/search/kickasstorrents.py:57`) cleanly vetoed at `0.908` after adding pagination/polling/worker-loop exemption.
- **Rejected before merge (Tier 4):**
  - `thin-wrapper` (AI-slop pass-through functions): failed both cost (`2,279` decisions across 4 repos) and precision (1/2 lab quiet FP at `0.955`, veto `0.100` on an `io.Closer` interface method).
  - `data-clump` (`Interface`, 20 decisions across 4 repos): passed the synthetic lab (1/1 `0.980`, 0/1 quiet) but hit 0/2 real-world precision in dogfood (`lintdec/oidc_browser.go:44` 2-param keyring store and `quack/packages/catalog/src/port.ts:166` already-bundled object port).

## Default-on promotion & opt-in graveyard pruning (2026-11-26)

Rebalanced the rule set from `15/45` default-on (`33%`) to `26/33` default-on (`79%`) after Clef verification across the 4 workspace repos (`lintdec`, `quack`, `games`, `home-server`; 980 files, 15,551 units, 0 errors):

- **Promoted to default-on (+11 rules, 359 combined decisions across all 980 files, 100% lab recall, 0 quiet FPs, 0 real FPs):**
  - Pre-filtered code/test/config/remediation rules: `lint-config-weakened` (2 decisions), `test-ignored-no-reason` (5), `md-no-remediation` (10), `test-over-mocked` (21), `hand-rolled-stdlib` (66), and `unbounded-retry` (69).
  - Pre-filtered ADR/RFC/tone rules: `md-adr-no-cost` (0 decisions on non-ADR repos; blind ADR/RFC Clef lab 1/1 loud `0.926`, veto `0.032`, 0/4 quiet), `md-adr-many-decisions` (0 decisions; lab 1/1 loud `0.988`, veto `0.040`, 0/4 quiet), `md-rfc-no-ask` (0 decisions; lab 1/1 loud `0.891`, veto `0.115`, 0/4 quiet), `md-one-sided-alternatives` (32 decisions), and `md-blames-reader` (154 decisions).
- **Pruned from opt-in graveyard (-12 rules; dead `Step` and `NounVerb` unit extractors removed):**
  - `iface-god-method`: 4/4 false positives on React `interface Props` in `games` (`0.846–0.969`, no veto), 0 real catches across 500 audited files.
  - `md-hidden-verb` (892 decisions across 4 repos, 0 above-floor catches in 500-file audit), `md-restates` (0.06% rate across 1,588 units), `md-prose-table` (0.19% rate), `md-one-topic` (0.17% rate), `md-marketing` (0 real hits), `md-howto-digression` (noisy on README/overview sections), `md-heading-promise` (noisy on terse ops headings), `md-noun-as-verb` (0 hits, too rare to measure), `md-topic-sentence` (0 above-floor hits), `md-step-not-command` (~60% FP on non-procedural lists without veto), and `comment-excuse` (0.18% rate, overlaps `todo-no-owner` and `comment-hedge`).
- **Remaining opt-in (`defaultOff`, 7 rules):** high-cost unfiltered code checks (`swallowed-error`, `incomplete-assertion`, `name-mismatch`, `stub-body`, `file-several-jobs`) plus repo-specific style checks (`comment-history`, `md-two-names`).

## OSS corpus hill-climbing (Clef-only, local read-only)

Evaluated default-on rules locally on Clef (`--protocol systemone --model clef --workers 1`, zero external PRs) across 3 compact AI-heavy/vibe-coded repos (`browser-use`, `nanoclaw`, `picoclaw`) and 2 mature baselines (`httpx`, `axios`):

- **Round 1** (7 rules: `lint-config-weakened`, `hand-rolled-stdlib`, `unbounded-retry`, `suppression-no-reason`, `test-ignored-no-reason`, `md-no-remediation`, `md-one-sided-alternatives`; 1,248 checks / 1,450 requests / 19m25s):
  - High-value true positives: `nanoclaw` 17/17 hand-rolled `parseArgs`/`parseFlags` functions (`hand-rolled-stdlib`) + infinite `for (;;)` SQLite retry loop (`src/state-sqlite.ts`, `unbounded-retry`); `picoclaw` `.golangci.yaml` disabling `errcheck`/`gosec`/`staticcheck` with `# TODO` (`lint-config-weakened`, `0.960`, veto `0.092`) + 3/3 unconditionally skipped dead tests out of 55 `t.Skip` calls (`test-ignored-no-reason`); real bare suppressions across all 5 repos.
  - Mature baselines (`httpx`, `axios`): 0 FPs on `hand-rolled-stdlib`, `unbounded-retry`, `test-ignored-no-reason`, `md-no-remediation`, `md-one-sided-alternatives`, `lint-config-weakened`.
- **Round 2** (`test-shared-state`, `test-over-mocked` on `httpx`, `axios`, `browser-use`; 313 checks / 347 requests / 3m04s):
  - `test-over-mocked`: 0/99 FPs on `httpx` + `axios`.
  - `test-shared-state`: 16/16 FPs on `axios` (`afterEach` cleanup hooks discarded by `splitTree`) and 129 wasted calls / 1 FP on `browser-use` (`monkeypatch.setenv` matched by `testSharedStateRe`).
- **Fixes verified live on Clef (before → after):**
  - Attach JS/TS `afterEach`/`beforeEach`/`afterAll`/`beforeAll` hooks to enclosing `TestFunction.Context` + exempt pure loopback URL helpers: `axios` `test-shared-state` FPs **16 → 0** (16 checks, 9s).
  - Drop `monkeypatch.setenv` from `testSharedStateRe` + unwrap Python `decorated_definition` (`@pytest.mark.skip`, `@pytest.mark.skipif`, `@patch`) + match `skipif`/`pytest.skip`: `browser-use` `test-shared-state` + `test-ignored-no-reason` checks **129 → 64**, FPs **1 → 0**, Python skip decorators now extracted.
  - Skip generated source files (`isGeneratedFile` header banner check): `picoclaw/web/frontend/src/routeTree.gen.ts` **1 → 0**.
  - Extract contiguous linter/typechecker `[tool.*]` sections from `pyproject.toml` for `LintConfig` (excluding test runners like `pytest`): `browser-use/pyproject.toml` (lines 62–98) scores `0.573` (veto `0.090`) vs. strict `httpx/pyproject.toml` (lines 98–115) `0.147` (veto `0.212`).
  - Unwrap JS/TS `expression_statement` for `TestFunction` doc attachment: comments directly above `it.skip(...)` now reach `Context` (`nanoclaw` `integration.test.ts` drops `0.918 → 0.092` once its 7-line `// SKIPPED:` rationale is visible).
  - Exempt runtime feature-detection stdlib fallbacks (`hand-rolled-stdlib`: `picoclaw` `generateSessionId` **1 → 0**, `nanoclaw` `register.ts` TP holds at `0.850`), API/architecture error reference tables (`md-no-remediation`: `picoclaw` `subturn.md` **1 → 0**), and test error-expectation blocks (`suppression-no-reason`: `httpx` test hits **21 → 13** while keeping **15/15** prod hits).
