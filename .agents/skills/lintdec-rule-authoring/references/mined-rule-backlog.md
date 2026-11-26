# Mined Candidate Rules Backlog (from Quack Maintainability Research)

Mined from four sealed `quack` research reports:
- `research:11d7345e` — Maintainability, Testability, Contributability, and CI/CD decomposition (~120 atomic signals, 45 verified sources)
- `research:5992a42b` — Enterprise Repository Scorecard prior art (OpenSSF, SonarQube, CodeScene, Google Tricorder, SPACE/DORA)
- `research:2e54cc89` — Atomic "Sloppiness" predicates (~70 facts across 12 categories)
- `research:3b1465b6` — High-quality CONTRIBUTING guides & AI contribution standards

Filtered through the 5 admission gates against `lintdec`'s current **37 rules** (`b98240f`).

---

## Tier A — High-ROI Candidates Ready for Synthetic Lab

All six pass the 5 admission gates, fit existing `Unit.Kind`s, and have cheap pre-filters (or narrow target kinds).

### 1. `test-ignored-no-reason` *(TestFunction · `warning` · ~99.5% pre-filtered)*
- **Source:** TSR Test Smell Catalog, Ignored Test (`https://tsr-catalog.readthedocs.io/en/latest/smells/ignored-test.html`) & Microsoft Lam et al., FaTB 2020 (`https://www.microsoft.com/en-us/research/wp-content/uploads/2020/07/LamETAL20FaTB.pdf`) — skipped tests rot permanently without a tracked issue or conditional guard.
- **Why semantic:** Regex finds `t.Skip` / `test.skip` / `@Disabled`, but platform/env capability guards (`if os.Getenv("QUACK_LIVE_IMAGE") == "" { t.Skip(...) }`, `if runtime.GOOS != "linux"`) are legitimate while bare permanent skips are debt.
- **Pre-filter (`filters.go`):** `(?i)\b(?:t\.Skip\w*|test\.skip|it\.skip|describe\.skip|xit|xtest|@Disabled|@Ignore|@pytest\.mark\.skip|unittest\.skip|skip\s*\()` — skips ~99.5% of tests for free.
- **Draft rubric:** *"The test is unconditionally skipped or disabled without naming a tracking ticket, owner, or concrete re-enable condition, leaving dead test coverage behind."*
- **Draft isolated veto:** *"Is the test in TARGET skipped only conditionally for a runtime environment requirement (such as OS, missing optional credential, or short mode) or tied to a named issue or ticket?"*

### 2. `test-over-mocked` *(TestFunction · `warning` · ~80% pre-filtered)*
- **Source:** Google SWE Book, Ch. 11 Testing Overview (`https://abseil.io/resources/swe-book/html/ch11.html`) — *"Test via public APIs"* / *"Test state, not interactions"*; also `sloppy-code` `T8` and coverage-gaming research (`repo-scorecard-prior-art`).
- **Why semantic:** Counting mock calls false-positives on legitimate boundary fakes (HTTP, DB, clock). Judging whether a test mocks out the subject under test itself or only verifies mock call choreography instead of observable behavior is pure semantic judgment — and a top AI-slop failure mode.
- **Pre-filter (`filters.go`):** `(?i)\b(?:vi\.fn|vi\.mock|vi\.spyOn|jest\.fn|jest\.mock|jest\.spyOn|MagicMock|patch\(|sinon\.|mock\.New|gomock|mockk|Mockito)\b` — skips non-mock tests for free.
- **Draft rubric:** *"The test mocks away the production behavior it claims to verify, or asserts only internal mock call choreography instead of observable outputs or state changes."*
- **Draft isolated veto:** *"Do the mocks or fakes in TARGET only replace external boundaries (such as network, filesystem, clock, or third-party services) while exercising real production logic and asserting real outcomes?"*
- **Lab check:** Must verify `tautology-test` does NOT already catch mock-choreography tests (subsumption test!).

### 3. `test-shared-state` *(TestFunction · `error` · lexical pre-filter)*
- **Source:** Google SWE Book, Ch. 11 Testing Overview (`https://abseil.io/resources/swe-book/html/ch11.html`) — *"Tests should assume as little as possible about the outside environment"* / *"they should not rely on a shared database"* (`S19`).
- **Why semantic:** Distinguishing isolated temp dirs/scoped env helpers (`t.TempDir()`, `t.Setenv()`, `mkdtemp`) from hardcoded shared paths (`/tmp/app.db`), mutated globals without teardown, or hardcoded TCP ports requires reading the cleanup/isolation pattern.
- **Pre-filter (`filters.go`):** Broad cue regex for filesystem/env/port/global mutation (`(?i)(?:/tmp/|os\.Setenv|process\.env\.|os\.environ|localhost:\d|127\.0\.0\.1:\d|WriteFile|writeFileSync|chmod|chdir)`), excluding pure unit tests.
- **Draft rubric:** *"The test writes to a fixed shared path, binds a fixed port, or mutates shared global or environment state without isolating or restoring it, allowing sibling or parallel tests to interfere."*
- **Draft isolated veto:** *"Does the test in TARGET isolate every file, port, or environment change (using a temp directory helper, ephemeral port, or explicit restore/cleanup) so it cannot leak into sibling tests?"*

### 4. `suppression-no-reason` *(Comment · `warning` · ~99.9% pre-filtered)*
- **Source:** Suppressions empirical evidence in `repo-scorecard-prior-art` (false positives are a minor share of suppressions; unjustified suppressions accumulate silent debt) + `maintainability-decompose` (`S12`/`S13`, waiver justification `R9`).
- **Why semantic:** Bare `// eslint-disable-next-line @typescript-eslint/no-explicit-any` or tautological `// @ts-ignore fix type` gives no rationale, whereas `// eslint-disable-next-line react-hooks/exhaustive-deps -- mount-only subscription` explains why it is safe.
- **Pre-filter (`filters.go`):** `(?i)(?:@ts-ignore|@ts-expect-error|eslint-disable|nolint|noqa|type:\s*ignore|@Suppress|biome-ignore|rubocop:disable)` — skips ~99.9% of comments for free.
- **Implementation caveat:** `structuralComment` in `split.go` currently strips tool directives (`toolDirective`). To support this rule without leaking directives into `comment-tautology` / `comment-long`, either emit a dedicated `Directive` unit kind or exclude `toolDirective` only from non-directive comment rules.
- **Draft rubric:** *"The linter, compiler, or type-checker suppression disables a check without stating a concrete reason why the suppressed code is safe or intentional."*
- **Draft isolated veto:** *"Does the suppression comment in TARGET state a concrete technical reason why the disabled check does not apply or why the bypass is safe?"*

### 5. `flag-argument` *(Function, Interface · `info` · ~95% pre-filtered)*
- **Source:** Refactoring.Guru, Primitive Obsession (`https://refactoring.guru/smells/primitive-obsession`) & Martin Fowler, Flag Argument (`https://martinfowler.com/bliki/FlagArgument.html`).
- **Why semantic:** Deterministic checks can spot a `bool`/`boolean` parameter, but setters (`setEnabled(bool)`) and options-struct fields are valid; a boolean flag that branches a function into two distinct behaviors (especially 2+ positional booleans like `sync(url, true, false)`) harms readability and testability.
- **Pre-filter (`filters.go`):** Signature line matches `(?i)\b(?:bool|boolean)\b|=\s*(?:true|false)\b` (for Go/TS/Python/Kotlin/Rust/Java) — skips ~95% of functions/interfaces.
- **Draft rubric:** *"The function or method takes a boolean flag parameter (or multiple positional booleans) that switches between two distinct behaviors instead of splitting into separate named functions or taking an options object."*
- **Draft isolated veto:** *"Is every boolean parameter in TARGET a natural on/off property of one single operation (such as a setter, toggle, or named options struct field) rather than a mode switch between two behaviors?"*

### 6. `md-no-remediation` *(Section · `warning` · ~95% pre-filtered)*
- **Source:** Google Tricorder (75% of analyzer bugs stemmed from unactionable wording) & OpenSSF Scorecard check docs (`https://github.com/ossf/scorecard/blob/main/docs/checks.md`) — every check/error doc pairs a risk/failure statement with explicit remediation steps.
- **Why semantic:** Troubleshooting and error-reference sections frequently describe symptoms or error codes without telling the reader how to fix, work around, or verify recovery.
- **Pre-filter (`filters.go`):** Section heading matches `(?i)^#{1,4}\s+.*(?:troubleshoot|error|warning|failure|debug|known issues|faq|caveat)` — skips ~95% of sections.
- **Draft rubric:** *"The troubleshooting or error section describes a failure, warning, or error state without stating a concrete remediation step, workaround, or diagnostic action the reader can take."*
- **Draft isolated veto:** *"Does the section in TARGET either give concrete steps to fix or work around the failure, link directly to remediation steps, or explain that no user action is required?"*

---

## Tier B — Worth Exploring Later (Higher Friction or Needs Splitter Work)

| Candidate | Kind | Source | Why Tier B |
|---|---|---|---|
| `data-clump` | `Interface` *(or `File` with param skeletons)* | Refactoring.Guru, Data Clumps (`https://refactoring.guru/smells/data-clumps`) | `Interface` works today (cheap, few units); `File` skeletons currently omit parameter lists, so file-wide clumps need a splitter extension. |
| `md-undocumented-env` | `Document` / `Section` | `maintainability-decompose` `C21`/`M28` | Pre-filter on `ENV_VAR=` / `$ENV_VAR` + setup heading; risk of false positives on reference snippets that deliberately point to `.env.example`. |
| `md-missing-verification` | `Section` | Diátaxis How-to / `maintainability-decompose` `C6` | Pre-filter on setup/install headings with fenced shell blocks; borderline actionability on short 1-command README install sections. |
| `error-context-lost` | `Function`, `Script` | Go Code Review Comments / `sloppy-code` `E8` | Overlaps partially with `swallowed-error` (which already covers rethrows dropping cause); bare `"failed"` messages may be noisy without careful floor tuning. |

---

## Explicitly Rejected Signals (Do NOT Add to `lintdec`)

Documented here so future rule-mining passes do not re-propose them:

| Research Signal | Why Rejected for `lintdec` | Where It Belongs |
|---|---|---|
| Unused vars/params/exports/files (`D1`–`D7`, `M19`), unreachable code (`D6`), empty functions (`D9`), self-compare/assign (`R1`–`R17`) | 100% deterministic AST/type/graph facts. | `tsc`, `eslint`, `knip`, `go vet` |
| Cyclomatic/cognitive complexity (`C2`–`C3`, `M1`–`M2`), duplication (`D11`–`D15`, `M3`–`M5`), import cycles & layers (`T9`–`T11`) | Deterministic/metric-based; `complex-method` was already removed (`tuning-fps.md:91`). | `sloplint`, `jscpd`, `madge`, `dependency-cruiser`, `ArchUnit` |
| Hardcoded ambient I/O / wall-clock / RNG in domain code (`T2`–`T7`) | Without a declared domain-vs-adapter path map, single-function LLM checks false-positive on every CLI/entrypoint. | `no-restricted-imports`, `no-restricted-globals`, `ArchUnit` |
| Stale feature flags / deprecation age / TODO age (`M16`, `M20`, `V14`) | Requires git blame / wall clock / rollout metadata; `comment-stale` had 0/17 precision (`tuning-fps.md:92`). | Repo scorecard / Piranha / blame script |
| Coverage %, mutation score, flaky retry rate, PR size, bus factor, churn bursts, OSV CVEs, branch protection (`S1`–`S18`, `M6`–`M15`, `C1`–`C20`, `CI1`–`CI20`, `P1`–`P12`) | Runtime, git-history, or Git-host API signals; several are also gameable proxies (`maintainability-decompose` §(c)). | Repo scorecard / CI gates / `osv-scanner` |
| `magic-numbers` (`R13`), `short-names` (`N1`), `comment-ratio` (`M3`) | High documented false-positive rates or gameable text ratios. | Language linter with allowlists (or excluded) |
