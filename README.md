# lintdec

lintdec uses a model to review code and Markdown for problems a pattern matcher can't judge, such as a test that cannot fail, a stale comment, or a sentence that buries a list. Every rule cites a primary source and links it; `--list-rules` shows the citation. Findings are suggestions, not proof; review them before editing.

## Build

From a checkout, with Go 1.27.3 or newer, cgo enabled, and a C compiler:

```sh
mkdir -p ~/.local/bin
go build -o ~/.local/bin/lintdec .
```

Add `~/.local/bin` to your `PATH` if needed.

## Set up

```sh
lintdec provider add
```

Choose **OpenRouter** and **Quick setup**. This selects the Jev decision-model setup and opens browser sign-in to create an API key. After a successful connection test, choose a model; press `/` to filter the list. To choose again, run `lintdec model openrouter` (use your provider name if different), or `lintdec model` for saved models. Set `LINTDEC_ACCESSIBLE=1` for plain prompts.

OpenRouter sends the code and document text being checked to OpenRouter. Use it only if your data policy permits. New keys are stored in the OS keychain; plaintext keys in older configs are not automatically migrated.

You can also use OpenAI or a local/custom endpoint. OpenRouter advanced setup supports chat models, existing keys, browser OIDC, and workload identity. See [authentication](docs/authentication.md) for setup, credential removal, and OIDC limits.

## Run

From the project you want to check:

```sh
lintdec .
```

A normal interactive run shows the top five findings; when there are more, the full report is saved in a private temporary file. Findings go to stdout; progress and cost go to stderr. For a saved report or a pipe:

```sh
lintdec --out jsonl --quiet . > findings.jsonl
```

Jev provides confidence, not a specific explanation or line: lintdec shows the rule summary and the start of the checked unit. Jev's high/med labels are based on confidence, not an independent assessment of impact.

Use `lintdec --list-rules` to see rules, `lintdec --rules flakey-test,md-prose-list .` to run only those two, and `lintdec -h` for options. To suppress a finding, put `//lintdec:ignore flakey-test` (or `<!-- lintdec:ignore md-prose-list -->` in Markdown) within two lines above it; use `--no-ignore` to audit suppressions.

## Project policy: lintdec.yaml

Commit a `lintdec.yaml` to the repository; lintdec finds it from the linted paths' common ancestor. `on:` adds a rule to the default set and `off:` removes one; `lintdec --list-rules` marks which rules are on by default.

```yaml
severity: warning
rules:
  - on: swallowed-error
  - off: md-prose-list
exclude:
  - '**/fixtures/**'
```

Settings resolve as flags, then `LINTDEC_*` environment variables, then `lintdec.yaml`, then `~/.config/lintdec/config.json`. Identity (URLs, API keys) never belongs in the yaml.

## Diff, budget, and CI

```sh
lintdec --diff origin/main
lintdec --budget 100 --severity warning --out jsonl --quiet . > findings.jsonl
```

`--diff` compares the working tree with the merge base, including untracked files not ignored by Git. It checks whole touched units, so findings can be on old lines; deletions can select adjacent units. Deleted files and symlinks are skipped, and an invalid or empty base fails.

`--budget` caps inference requests, including retries and follow-ups, not dollars. An incomplete budget-limited run exits 2. Fatal auth or billing responses (HTTP 401/402/403) also stop remaining checks and exit 2; re-run after fixing credentials or balance to resume from cache. Each rule has a fixed level (`error`, `warning`, or `info`); `--severity warning` reports warnings and errors.

Each rule also has a confidence floor, and System One runs ask a rule's exception as a separate isolated request only once the rule itself triggers (lowering `--threshold` below `0.50` may therefore trigger extra veto requests). Chat models run on floors alone; passing `--threshold` explicitly overrides per-rule floors while keeping vetoes active.

Cost is provider-reported, a known subtotal, or `unknown`; OpenRouter may omit separate BYOK charges. Cached checks avoid model calls.

Exit codes:

- `0`: no findings at the chosen severity
- `1`: findings reported
- `2`: error or incomplete run
- `130`: interrupted

Some checks can fail even when the exit code is 0 or 1; check stderr. Ctrl-C flushes findings collected so far.

## Scope and safety

Normal directory scans do not apply `.gitignore`. Use `--exclude` or `lintdec.yaml` to keep files out; explicitly named files bypass include/exclude filters. Only supported source types and Markdown are scanned by default. Files over 1 MiB and Git worktree directories are skipped; pass explicit files for a worktree. Markdown front matter and fenced code are not checked as prose.

## Editor

[editors/vscode](editors/vscode/README.md) shows findings in VS Code as squiggles and in the Problems panel.

Reports and verdicts cached under `~/.cache/lintdec/cache` can contain sensitive information; protect them. Use `--no-cache` to bypass the cache, and don't put credentials in `lintdec.yaml`.

## Adding rules

See [.agents/skills/lintdec-rule-authoring/SKILL.md](.agents/skills/lintdec-rule-authoring/SKILL.md) for the rule admission gates, pre-filter/veto authoring guide, blind synthetic recall lab, and multi-repo promotion process.
