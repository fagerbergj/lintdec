# Mined Candidate Rules Backlog

Candidate rules mined from published software-maintainability, testability, static-analysis, and repository-health primary sources (Google SWE Book, TestSmells & TSR Catalog, OpenSSF Scorecard, Google Tricorder, `eslint-comments` / Hu et al. FSE 2025, Refactoring.Guru, Martin Fowler, Diátaxis), filtered through the 5 admission gates against `lintdec`'s current **37 rules** (`b98240f`).

---

## Tier A — High-ROI Candidates Ready for Synthetic Lab

All five pass the 5 admission gates, fit existing `Unit.Kind`s with zero splitter changes, and pair with cheap RE2 pre-filters. Pre-filter savings below are initial estimates until measured via `--dump-units`.

### 1. `test-ignored-no-reason` *(TestFunction · `warning` · est. ~99% pre-filtered)*
- **Source:** TestSmells Catalog, Ignored Test (`https://testsmells.org/pages/testsmells.html`) & TSR Test Smell Catalog (`https://tsr-catalog.readthedocs.io/en/latest/smells/ignored-test.html`) — skipped tests rot permanently without a tracked issue or conditional guard.
- **Why semantic:** Regex finds `t.Skip` / `test.skip` / `@Disabled`, but platform/env capability guards (`if runtime.GOOS != "linux"`) are legitimate while bare permanent skips are debt.
- **Pre-filter (`filters.go`):** `(?i)(?:^|[^-\w])(?:t\.Skip\w*|test\.skip|it\.skip|describe\.skip|xit|xtest|@Disabled|@Ignore|@pytest\.mark\.skip|unittest\.skip)(?:[^-\w]|$)`
- **Draft rubric:** *"The test is unconditionally skipped or disabled without naming a tracking ticket, owner, or concrete re-enable condition, leaving dead test coverage behind."*
- **Draft isolated veto:** *"Is the test in TARGET skipped only conditionally for a runtime environment requirement (such as OS, missing optional credential, or short mode) or tied to a named issue or ticket?"*

### 2. `test-over-mocked` *(TestFunction · `warning` · est. ~80% pre-filtered)*
- **Source:** Google SWE Book, Ch. 11 Testing Overview (`https://abseil.io/resources/swe-book/html/ch11.html`) — *"Test via public APIs"* / *"Test state, not interactions"*.
- **Why semantic:** Counting mock calls false-positives on legitimate boundary fakes (HTTP, DB, clock). Judging whether a test mocks out the subject under test itself or only verifies mock call choreography instead of observable behavior is pure semantic judgment.
- **Pre-filter (`filters.go`):** `(?i)\b(?:vi\.fn|vi\.mock|vi\.spyOn|jest\.fn|jest\.mock|jest\.spyOn|MagicMock|create_autospec|sinon\.|gomock|mockk|Mockito|unittest\.mock)\b|\bpatch\s*\(`
- **Draft rubric:** *"The test mocks away the production behavior it claims to verify, or asserts only internal mock call choreography instead of observable outputs or state changes."*
- **Draft isolated veto:** *"Do the mocks or fakes in TARGET only replace external boundaries (such as network, filesystem, clock, or third-party services) while exercising real production logic and asserting real outcomes?"*
- **Lab check:** Must verify `tautology-test` does not already subsume mock-choreography tests.

### 3. `test-shared-state` *(TestFunction · `error` · lexical pre-filter)*
- **Source:** Google SWE Book, Ch. 11 Testing Overview (`https://abseil.io/resources/swe-book/html/ch11.html`) — *"Tests should assume as little as possible about the outside environment"* / *"they should not rely on a shared database"*.
- **Why semantic:** Distinguishing isolated temp dirs/scoped env helpers (`t.TempDir()`, `t.Setenv()`, `mkdtemp`) from hardcoded shared paths (`/tmp/app.db`), mutated globals without teardown, or hardcoded TCP ports requires reading the cleanup/isolation pattern.
- **Pre-filter (`filters.go`):** `(?i)(?:/tmp/|os\.Setenv|process\.env\.|os\.environ|localhost:\d|127\.0\.0\.1:\d|WriteFile|writeFileSync|os\.Chdir|os\.chdir|process\.chdir)`
- **Draft rubric:** *"The test writes to a fixed shared path, binds a fixed port, or mutates shared global or environment state without isolating or restoring it, allowing sibling or parallel tests to interfere."*
- **Draft isolated veto:** *"Does the test in TARGET isolate every file, port, or environment change (using a temp directory helper, ephemeral port, or explicit restore/cleanup) so it cannot leak into sibling tests?"*

### 4. `flag-argument` *(Function, Interface · `info` · est. ~95% pre-filtered)*
- **Source:** Martin Fowler, Flag Argument (`https://martinfowler.com/bliki/FlagArgument.html`) & Refactoring.Guru, Primitive Obsession (`https://refactoring.guru/smells/primitive-obsession`).
- **Why semantic:** Checking only the parameter list (`(...)` before `{`) skips boolean-returning predicates (`IsReady() bool`), and the model distinguishes valid setters (`SetEnabled(bool)`) from boolean flags that switch a function between two distinct behaviors.
- **Pre-filter (`filters.go`):** Parameter list matches `(?i)\b(?:bool|boolean)\b|=\s*(?:true|false)\b`.
- **Draft rubric:** *"The function or method takes a boolean flag parameter (or multiple positional booleans) that switches between two distinct behaviors instead of splitting into separate named functions or taking an options object."*
- **Draft isolated veto:** *"Is every boolean parameter in TARGET a natural on/off property of one single operation (such as a setter, toggle, or options struct field) rather than a mode switch between two behaviors?"*

### 5. `md-no-remediation` *(Section · `warning` · est. ~95% pre-filtered)*
- **Source:** Google Tricorder (`https://research.google/pubs/tricorder-building-a-program-analysis-ecosystem/`) & OpenSSF Scorecard check docs (`https://github.com/ossf/scorecard/blob/main/docs/checks.md`) — error and check docs must pair each failure with actionable remediation steps.
- **Why semantic:** Troubleshooting and error-reference sections frequently describe symptoms or error codes without telling the reader how to fix, work around, or verify recovery.
- **Pre-filter (`filters.go`):** Section heading matches `(?i)^#{1,4}\s+.*(?:troubleshoot|error|warning|failure|debug|known issues|faq|caveat)`.
- **Draft rubric:** *"The troubleshooting or error section describes a failure, warning, or error state without stating a concrete remediation step, workaround, or diagnostic action the reader can take."*
- **Draft isolated veto:** *"Does the section in TARGET either give concrete steps to fix or work around the failure, link directly to remediation steps, or explicitly explain that no user action is required?"*

---

## Tier B — Needs Splitter Work or Careful Scope Validation

| Candidate | Kind | Source | Why Tier B |
|---|---|---|---|
| `suppression-no-reason` | Dedicated `Directive` kind *(splitter change)* | `eslint-comments/require-description` (`https://eslint-community.github.io/eslint-plugin-eslint-comments/rules/require-description.html`) & Hu et al., FSE 2025 (`https://doi.org/10.1145/3715729`) | `structuralComment` in `split.go` currently drops tool directives before `buildJobs`, so this requires re-kinding suppression comments as `Directive` units. **Gate 1 note:** bare directives with zero trailing text can be checked deterministically, while judging whether trailing prose is a concrete rationale vs. a tautological excuse (`// @ts-ignore fix type error`) is semantic. |
| `data-clump` | `Interface` *(or `File` with param skeletons)* | Refactoring.Guru, Data Clumps (`https://refactoring.guru/smells/data-clumps`) | `Interface` works today (cheap, few units); `File` skeletons currently omit parameter lists, so file-wide clumps need a splitter extension. |
| `md-undocumented-env` | `Document` / `Section` | Twelve-Factor / Dev Containers (`https://containers.dev/`) | Pre-filter on `ENV_VAR=` / `$ENV_VAR` + setup heading; risk of false positives on snippets that intentionally point to `.env.example`. |
| `md-missing-verification` | `Section` | Diátaxis How-to (`https://diataxis.fr/how-to-guides/`) | Pre-filter on setup/install headings with fenced shell blocks; borderline actionability on short 1-command install sections. |
| `error-context-lost` | `Function`, `Script` | Go Code Review Comments (`https://go.dev/wiki/CodeReviewComments#handle-errors`) | Overlaps partially with `swallowed-error` (which already covers rethrows dropping cause); bare `"failed"` messages may be noisy without floor tuning. |

---

## Explicitly Rejected Signals (Do NOT Add to `lintdec`)

Documented here so future rule-mining passes do not re-propose them:

| Research Signal | Why Rejected for `lintdec` | Where It Belongs |
|---|---|---|
| Unused vars/params/exports/files, unreachable code, empty functions, self-compare/assign (`tsc`, ESLint, `knip`: `https://knip.dev/`) | 100% deterministic AST/type/graph facts. | `tsc`, `eslint`, `knip`, `go vet` |
| Cyclomatic/cognitive complexity, duplication (`jscpd`: `https://github.com/kucherenko/jscpd`), import cycles & layers (`ArchUnit`: `https://www.archunit.org/userguide/html/000_Index.html`) | Deterministic/metric-based; `complex-method` was already removed (`tuning-fps.md:91`). | `sloplint`, `jscpd`, `madge`, `dependency-cruiser`, `ArchUnit` |
| Hardcoded ambient I/O / wall-clock / RNG in domain code (ESLint `no-restricted-imports`: `https://eslint.org/docs/latest/rules/no-restricted-imports`) | Without a declared domain-vs-adapter path map, single-function LLM checks false-positive on every CLI/entrypoint. | `no-restricted-imports`, `no-restricted-globals`, `ArchUnit` |
| Stale feature flags / deprecation age / TODO age (Uber Piranha: `https://github.com/uber/piranha`) | Requires git blame / wall clock / rollout metadata; `comment-stale` had 0/17 precision (`tuning-fps.md:92`). | Repo scorecard / Piranha / blame script |
| Coverage %, mutation score (`https://arxiv.org/abs/2102.11378`), flaky retry rate, PR size, bus factor (`https://homepages.dcc.ufmg.br/~mtov/pub/2019-sqj.pdf`), churn bursts, OSV CVEs (`https://google.github.io/osv.dev/api/`), branch protection | Runtime, git-history, or Git-host API signals; several are also gameable proxies (Google SWE Book Ch. 11: coverage targets become ceilings). | Repo scorecard / CI gates / `osv-scanner` |
| Magic numbers (`https://eslint.org/docs/latest/rules/no-magic-numbers`), short identifier names, comment-to-code ratio | High documented false-positive rates or gameable text ratios. | Language linter with allowlists (or excluded) |
