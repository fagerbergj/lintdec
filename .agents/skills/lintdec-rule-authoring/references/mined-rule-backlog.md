# Mined Candidate Rules Backlog

Candidate rules mined from published software-maintainability, testability, static-analysis, and repository-health primary sources (Google SWE Book, TestSmells & TSR Catalog, OpenSSF Scorecard, Google Tricorder, `eslint-comments` / Hu et al. FSE 2025, Refactoring.Guru, Martin Fowler, Diátaxis), filtered through the 5 admission gates against `lintdec`'s current **42 rules** (`2d21140`).

---

## Shipped in `#31` (`2d21140`)

Five mined rules passed the blind synthetic recall lab and 4-repo dogfood (`99.0%` combined pre-filter reduction; details in `tuning-fps.md`):

| Rule | Status | Kind | Source |
|---|---|---|---|
| `test-shared-state` | Default-on (`error`) | `TestFunction` | Google SWE Book Ch. 11 (`https://abseil.io/resources/swe-book/html/ch11.html`) |
| `suppression-no-reason` | Default-on (`warning`) | `Directive` | `eslint-comments/require-description` (`https://eslint-community.github.io/eslint-plugin-eslint-comments/rules/require-description.html`) & Hu et al., FSE 2025 (`https://doi.org/10.1145/3715729`) |
| `test-ignored-no-reason` | Opt-in (`warning`) | `TestFunction` | TestSmells (`https://testsmells.org/pages/testsmells.html`) & TSR Catalog (`https://tsr-catalog.readthedocs.io/en/latest/smells/ignored-test.html`) |
| `test-over-mocked` | Opt-in (`warning`) | `TestFunction` | Google SWE Book Ch. 11 (`https://abseil.io/resources/swe-book/html/ch11.html`) |
| `md-no-remediation` | Opt-in (`warning`) | `Section` | Google Tricorder (`https://research.google/pubs/tricorder-building-a-program-analysis-ecosystem/`) & OpenSSF Scorecard (`https://github.com/ossf/scorecard/blob/main/docs/checks.md`) |

---

## Tier B — Remaining Candidates (Needs Splitter Work or Scope Validation)

| Candidate | Kind | Source | Why Tier B |
|---|---|---|---|
| `data-clump` | `Interface` *(or `File` with param skeletons)* | Refactoring.Guru, Data Clumps (`https://refactoring.guru/smells/data-clumps`) | `Interface` works today (cheap, few units); `File` skeletons currently omit parameter lists, so file-wide clumps need a splitter extension. |
| `md-undocumented-env` | `Document` / `Section` | Twelve-Factor / Dev Containers (`https://containers.dev/`) | Pre-filter on `ENV_VAR=` / `$ENV_VAR` + setup heading; risk of false positives on snippets that intentionally point to `.env.example`. |
| `md-missing-verification` | `Section` | Diátaxis How-to (`https://diataxis.fr/how-to-guides/`) | Pre-filter on setup/install headings with fenced shell blocks; borderline actionability on short 1-command install sections. |
| `error-context-lost` | `Function`, `Script` | Go Code Review Comments (`https://go.dev/wiki/CodeReviewComments#handle-errors`) | Overlaps partially with `swallowed-error` (which already covers rethrows dropping cause); bare `"failed"` messages may be noisy without floor tuning. |

---

## Explicitly Rejected Signals (Do NOT Add to `lintdec`)

Documented here so future rule-mining passes do not re-propose them:

| Research Signal | Why Rejected for `lintdec` | Where It Belongs |
|---|---|---|
| `flag-argument` — boolean parameters (Martin Fowler: `https://martinfowler.com/bliki/FlagArgument.html`, Refactoring.Guru: `https://refactoring.guru/smells/primitive-obsession`) | Passed synthetic lab (2/2, 0/2) but produced 66 false positives across 4 repos on normal single-boolean domain parameters (`isSoft`, `isSplitHand`, `dealerHitsSoft17`, `allowPort`, `reveal`); dropped before merge (`tuning-fps.md`). | API design review (or excluded) |
| Unused vars/params/exports/files, unreachable code, empty functions, self-compare/assign (`tsc`, ESLint, `knip`: `https://knip.dev/`) | 100% deterministic AST/type/graph facts. | `tsc`, `eslint`, `knip`, `go vet` |
| Cyclomatic/cognitive complexity, duplication (`jscpd`: `https://github.com/kucherenko/jscpd`), import cycles & layers (`ArchUnit`: `https://www.archunit.org/userguide/html/000_Index.html`) | Deterministic/metric-based; `complex-method` was already removed (`tuning-fps.md:91`). | `sloplint`, `jscpd`, `madge`, `dependency-cruiser`, `ArchUnit` |
| Hardcoded ambient I/O / wall-clock / RNG in domain code (ESLint `no-restricted-imports`: `https://eslint.org/docs/latest/rules/no-restricted-imports`) | Without a declared domain-vs-adapter path map, single-function LLM checks false-positive on every CLI/entrypoint. | `no-restricted-imports`, `no-restricted-globals`, `ArchUnit` |
| Stale feature flags / deprecation age / TODO age (Uber Piranha: `https://github.com/uber/piranha`) | Requires git blame / wall clock / rollout metadata; `comment-stale` had 0/17 precision (`tuning-fps.md:92`). | Repo scorecard / Piranha / blame script |
| Coverage %, mutation score (`https://arxiv.org/abs/2102.11378`), flaky retry rate, PR size, bus factor (`https://homepages.dcc.ufmg.br/~mtov/pub/2019-sqj.pdf`), churn bursts, OSV CVEs (`https://google.github.io/osv.dev/api/`), branch protection | Runtime, git-history, or Git-host API signals; several are also gameable proxies (Google SWE Book Ch. 11: coverage targets become ceilings). | Repo scorecard / CI gates / `osv-scanner` |
| Magic numbers (`https://eslint.org/docs/latest/rules/no-magic-numbers`), short identifier names, comment-to-code ratio | High documented false-positive rates or gameable text ratios. | Language linter with allowlists (or excluded) |
