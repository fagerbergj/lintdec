# lintdec plan

Roadmap for `lintdec`, a decision-model code linter. Each slice is vertical: it ends with something runnable and a verification step that proves it.

Status: `[x]` verified · `[~]` code written, unverified · `[ ]` not started.

## Slices

### 1. CLI base [x]

Dispatch, flags, help, `--list-rules`, exit codes 0/1/2. Flags work before or after positionals (reorderArgs).

- Verify: `go build ./...` and `go vet ./...` clean; `lintdec -h` prints usage, exit 2; `lintdec --list-rules` prints all rule ids; `lintdec bad-flag` → usage, exit 2.
- Done when: the command surface is stable — later slices only add flags.

### 2. Provider config and connection [x]

`lintdec provider list|add|use|rm|test`, wizard, `~/.config/lintdec/config.json` (0600), connection test via `/models` with completion fallback. Verified: add/list/use/rm (incl. `--force` guard) against both local endpoints.

- Done when: a fresh machine can go from zero to a working provider with one wizard.

### 3. Locate and split [x]

Glob positionals (files/dirs/`**` patterns), include/exclude, gitignore-free walker, go/ast + markdown splitters, oversized-unit digests and windows. `--dump-units` prints units/digests with no model call — the debug seam for every later slice. Verified: 128 units on self, 1756 across quack/home-server/wt-prompt; digest path exercised by the 375-line lintCmd (complex-method found it via the digest). Multi-language tree-sitter splitter landed after v1 (see below): TS/JS/Python/Rust/Bash/Java/C/C++/C#/Kotlin/Swift, Go still on go/ast.

- Done when: the unit list for any tree is inspectable and line spans check out by eye.

### 4. Apply rules, one at a time [x]

Runner: jobs = unit × applicable rule, worker pool, provider call, verdict parse, cache, budget, `--budget` and `--rules` as throttles. Batching: one model call per unit decides all applicable rules (keyed by rule id); omitted keys get one follow-up call, then a fresh single-rule call. 429s get long backoff; the first 5 errors print to stderr. Verified: 392 self-decisions and 3028 cross-repo decisions, 0 errors. Per-rule planted/clean fixtures still owe to slice 9.

### 5. All rules + big units [x]

All rules live on real trees; digest path exercised by a 375-line function. Remaining: a dedicated >200-line test fixture with hand-checked line anchors.

### 6. Output, severity, ledger [x]

text/json/jsonl, `--severity` filter (decided-but-suppressed counted in the summary so numbers reconcile), `--ledger FILE` append-as-decided. Verified: jsonl parseable, ledger == stdout (223 == 223 on the cross-repo run), findings sorted severity → confidence → location. Verdicts carry a 0-1 confidence; the model currently clusters at ~0.95 (calibration item).

### 7. Repo-level config and inline ignore [x]

`lintdec.yaml` (severity/rules include-exclude/model/provider pin), precedence flag > env > yaml > global config, `//lintdec:ignore rule-id` within 2 lines, `--no-ignore` to audit. Verified: yaml `- off: md-para-long` drops the rule (banner shows 19); isolated blank-line fixture proves deterministic suppression (1 ignored-inline) and `--no-ignore` restores the finding. Side effect: the model also respects visible directives on its own — a bonus, not the mechanism. Yaml discovery anchors to the linted paths' common ancestor, not the CWD, so `lintdec quack` from anywhere finds quack's policy.

### 8. Robustness: failures and interruption [x]

Deferred until slice 9; now verified.

- Dead port (7-check target, nothing on the port): run completes, 7/7 errors reported, exit 2, per-rule keys in `.lintdec/errored.json`.
- `--retry-errors` round-trip: after the dead-port run, exactly the 7 failed checks re-ran (0 cached, 2s, exit 0), error file consumed. First attempt exposed a refactor leftover: the filter compared batch-shaped keys against per-rule keys.
- Ctrl-C mid-run: findings flushed to stdout (21/36), exit 130, resume run 0s (73/73 cached). First attempt exposed the send loop filling the channel before SIGINT lands, so interrupted stayed false; now derived from the context.
- 429 storm during a 16-worker run: backoff absorbed it, run completed.

### 9. Calibration + dogfood [x]

DONE: seeded-defect calibration. 22 plants (one per rule) + clean twin in `lint-targets/dotagents/lab/` with the ledger in `lab/planted.md`. Result: 22/22 caught on two consecutive fresh runs (`--no-cache`), zero false positives, clean twin quiet both times. First run: 21/22 — complex-method missed a weak plant (45-line single-purpose function); plant strengthened to ~90 lines mixing flags/webhook-retry/formatting/IO and caught immediately. Also added this session: `md-hard-wrap` and `md-table-dense` rules (user request: hard line-wrapping, dense table cells) — both caught on their plants; table work exposed a splitter double-append bug (tables were also registered as Paragraph units) now fixed.

Done earlier: full 223-finding review — 22 valid, 1 weak, 9 fixture (now excluded by quack's lintdec.yaml), 105 worktree duplicates and 8 unit-shape phantoms removed by tool fixes; md-jargon redefined (118 → 1 findings). Residual (moved to polish): confidence still clusters at 0.95 (weak as a sort key, harmless as a gate); one borderline md-jargon survivor (quack slots.md:8).

### 10. Performance sizing + README [x]
Done. Measured (local 27B behind llama-swap): ~370-515 checks/min on small prose units at 8 workers, ~3.5 min cold for a 1,200-check repo, ~0s warm. Data-backed conclusion: defaults stand as-is — `rawBudgetLines=200` (proven by the 375-line digest case) and `--workers 8` (16 caused the 429 storm; the limit is the gateway, not vLLM). README.md written from real dogfood usage: install, provider wizard, first run, all 22 rules grouped, lintdec.yaml (quack's real fixture exclusion as the example), inline suppression, cache semantics, CI (exit codes, jsonl, retry-errors, budget, ledger), measured perf, calibration-lab re-run recipe, known limits. Verify step run: every README command executed as written from a fresh shell (install is `go build -o ~/.local/bin/lintdec`).
- Done when: a new user follows the README to a first finding unaided.

### Multi-language splitters [x]

Post-v1 add, landed once tree-sitter was accepted as the right answer. `treeSplit.go`: one generic tree-sitter splitter parameterized per language (function-like node types, interface-like node types, test detection via `test_*` names and `test()/it()/describe()` calls, doc-comment runs with marker stripping, Python docstrings, `const fn = () =>` whole-declaration units, export_statement wrappers kept in the span). 11 grammars: TS, JS, Python, Rust, Bash, Java, C, C++, C#, Kotlin, Swift. Go stays on go/ast (exact, battle-tested). Cost accepted: cgo + gcc at build time (one dependency: smacker/go-tree-sitter); the binary stays self-contained. Verified: unit tests (TS export + doc attachment, Python docstring / test_ detection) and real-tree dumps (quack boundary.ts: 11 units incl. File skeleton; check-suite.py: 10 units); smoke lint on TS + Python produced a plausible complex-method finding. Bugs caught by probes: Node.String() returns S-expressions (Content(src) is the text API), export_statement wrapping broke doc attachment and the skeleton, double-emit from wrapper + inner.

### Tuning and dogfood PRs [x]

Collected the false positives from the first full lint pass, tuned the rubric, and fixed every surviving finding.
- Four rubric patches (md-table-dense audience + ~25-word cell floor, comment-tautology mechanism clause, md-jargon same-file definition + product-names clauses, md-hard-wrap single-line + mid-sentence discriminator) plus code floors: single-line units skip md-hard-wrap, paragraphs of at most 80 words skip md-para-long, fewer than two mid-sentence line breaks skip md-hard-wrap, findings anchor to the unit's real span (the model's line number is unreliable).
- Recalibrated after each change: 22/22 loud plants, six quiet plants silent, twice in a row.
- Fixed all findings across the active repos and opened four PRs, each labeled quack:review, separated by repo: games #42, dotagents #9, home-server #31, quack #2452. NightsOut linted clean, no PR. quack's .quack/v1-design is gitignored; its fixes live on disk.
- Fix patterns that proved out: reflow wrapped prose to one sentence per line, split multi-topic paragraphs at sentence boundaries, and move table-cell rationale to notes below the table.

### Research-driven rules [x]

Quack's CLI research run ("what makes code sloppy, decomposed into the smallest predicates", ~70 atomic facts) was filtered against the model-only, per-unit design: (a)-class facts belong to ESLint/tsc/OSV, (b)-class thresholds are forbidden by the no-math rule, cross-unit smells (synonymy, duplication, feature envy) don't fit one unit. Four judgment-style rows became rules 23-26: `name-mismatch` (name promises the opposite of the behavior, N8), `swallowed-error` (catch drops the error the caller needs, E2/E3), `stub-body` (placeholder body under a promising name, D9/D10), `commented-code` (an earlier version of the code left in comments, D8). Calibrated 26/26 on two consecutive fresh runs, zero extras, quiet plants silent.

### Judgment-only correction [x]

The numeric floors were wrong by the standing rule: the 80-word code floor for md-para-long and the "N sentences/words/lines/parameters" anchors in five rubrics let the model count instead of read, and the dotagents PR review surfaced the resulting tension (one-sentence-per-paragraph splits fighting the prose-rhythm guidance). Removed every number from the rubrics and the code; md-para-long now turns on a paragraph packing separate topics a reader must reorient between. Re-calibrated 26/26 twice, quiet plants silent; the three repos went from 33 spurious dense-paragraph findings back to 5, of which 1 is a genuine mixed-topic paragraph left as a finding.

## Standing rules


- One dependency (smacker/go-tree-sitter, cgo; gcc needed at build time); keep it to one.
- Model-only, no math: rules are judgment ("a human would say so"), never size thresholds. No word/sentence/line/parameter counts in rubrics or code — the one exception is the mid-sentence-break check in md-hard-wrap, which defines where line breaks fall rather than how big anything is.
- One model call = one unit; findings anchor to real unit spans or are dropped.
- Every slice ends with its Verify steps run, not just its code written.
- `go build ./... && go vet ./...` green before a slice is marked [x].
- No hard line-wrapping in prose: one sentence per line.
