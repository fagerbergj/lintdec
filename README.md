# lintdec

A decision-model code linter. A deterministic splitter breaks your tree into units (functions, tests, interfaces, comment blocks, paragraphs, bullets, tables), and a local model decides, per unit, whether each rule's rubric is violated. Findings carry severity and a 0-1 confidence, and are sorted most-actionable first.

It exists because some things a linter should catch are too subtle for pattern matching: a test that cannot fail, a comment describing code that no longer does that, an interface leaking its storage layer. Those are judgments. This runs the judgment on your own hardware against any OpenAI-compatible endpoint.

## Install

```sh
cd lintdec
go build -o ~/.local/bin/lintdec
```

cgo + a C compiler are required to build (the tree-sitter grammars compile C at build time); the finished binary is self-contained. No runtime dependencies. First build compiles ~11 grammar C sources and takes a few minutes; later builds are fast.

## First run

Add a provider once (wizard):

```sh
lintdec provider add
# name [local]: clef
# base URL [http://localhost:11434/v1]: http://127.0.0.1:11436/v1
# model id: qwen3.8-27b
# test the connection now? [y]:
```

Then lint:

```sh
lintdec                        # current dir
lintdec pkg/ docs/             # dirs
lintdec 'pkg/**/*_test.go'     # glob positionals
lintdec --rules flakey-test,tautology-test .
lintdec --severity med --out jsonl pkg/ | jq -c .
```

Flags may go before or after paths. Precedence: flag > env (`LINTDEC_*`) > `lintdec.yaml` > `~/.config/lintdec/config.json`.

## Rules

26 rules, list them with `lintdec --list-rules`.

- **Tests:** `flakey-test`, `tautology-test`, `incomplete-assertion`, `wrong-assertion`, `test-except-pass`, `assert-self`
- **Comments:** `comment-long`, `comment-tautology`, `comment-excuse`, `comment-stale`, `comment-decision-record`, `comment-hedge`, `comment-restates-name`, `todo-no-owner`
- **Prose:** `md-para-long`, `md-jargon`, `md-hard-wrap`, `md-table-dense`
- **Structure:** `iface-leaky`, `iface-god-method`, `complex-method`, `big-file`
- **Implementation:** `name-mismatch`, `swallowed-error`, `stub-body`, `commented-code`

Code languages are split with tree-sitter: Go (via `go/ast`), TypeScript, JavaScript, Python, Rust, Bash, Java, C, C++, C#, Kotlin, Swift. Markdown is always understood. Tests are recognized as `*_test.go`, `test_*` functions, and `test()/it()/describe()` blocks. Other languages contribute their markdown until their grammar is added (one entry in `treeSplit.go`).

## Project policy: lintdec.yaml

Committed per repo, found from the linted paths' common ancestor:

```yaml
severity: med
rules:
  - off: md-jargon
exclude:
  - '**/fixtures/**'
```

Real example from quack:

```yaml
exclude:
  - '**/fixtures/**'
  - '**/test-fixtures/**'
```

Identity (URLs, API keys) never belongs here — it lives in `~/.config/lintdec/config.json`. The yaml can only name providers/models.

## Suppressing a finding

Within 2 lines above the finding:

```go
//lintdec:ignore flakey-test  // the timer IS the test
func TestDrainsUnderLoad(t *testing.T) {
```

`<!-- lintdec:ignore md-para-long -->` in markdown. `--no-ignore` audits suppressed findings.

## Cache and re-runs

Verdicts are cached per (unit, rule, model, temperature, prompt version) in `~/.cache/lintdec/cache`, keyed on absolute paths. Consequences:

- Re-running is free; editing a file re-decides only that file.
- Changing a rubric re-decides only the affected rule (the prompt hash is in the key).
- Running `--rules a,b` then `--rules a` costs nothing the second time — checks, not batches, are cached.
- `rm -rf ~/.cache/lintdec/cache` forces a full re-decide.

## CI

Exit codes: `0` clean, `1` findings at or above `--severity`, `2` error (all checks failed = broken provider).

```sh
lintdec --severity med --out jsonl --quiet . > findings.jsonl || cat findings.jsonl
lintdec --retry-errors        # after a flaky network run: only the failed checks
```

`--budget N` caps model calls (dry-run guard); `--ledger FILE` appends findings as decided so a killed run loses nothing. Ctrl-C flushes findings and exits 130.

## Performance (measured, local 27B behind llama-swap)

- Small prose units: ~370-515 checks/min at the default 8 workers.
- A 1,200-check repo: ~3.5 min cold, ~0s warm.
- The gateway (llama-swap) 429s above ~8-16 concurrency; the built-in backoff absorbs bursts, but don't chase it — `--workers 8` is the measured sweet spot.
- vLLM itself is fine with more queued requests; the limit is the gateway, not the GPU.

## Calibration lab

`../lint-targets/dotagents/lab/` holds 26 seeded defects (one per rule) plus a clean twin, with the expected-findings ledger in `lab/planted.md`. Re-run after any rubric change:

```sh
lintdec lint-targets/dotagents/lab --no-cache --out jsonl
# expect: all 26 rules present, no rule outside the ledger
```

Last known state: 26/26 caught on two consecutive fresh runs, zero false positives, clean twin quiet.

## Known limits

- Model confidence clusters near 0.95 — fine as a gate, weak as a sort key.
- Non-Go tree-sitter units are parser-accurate spans, but doc-comment attachment is a proximity heuristic; Go units are exact via `go/ast`.
- `md-jargon` judges audience from the file; it is deliberately quiet on expert/operational notes and internal design docs.
- Worktrees (`.git` file, not dir) are skipped with a warning to avoid duplicate findings; pass explicit files to lint one.
- Tables, front matter, and code fences are not linted as prose.
