# lintdec for VS Code

Shows [lintdec](https://github.com/fagerbergj/lintdec) findings in Markdown files as squiggles and in the Problems panel. Each finding links its rule to the published style guidance it enforces.

It lints a Markdown file when you save it, or on demand with **lintdec: Lint current file**. Paragraphs already checked come from lintdec's cache, so only edited text costs model calls.

## Install

```sh
go build -o ~/.local/bin/lintdec .          # from the repository root
cd editors/vscode && npx @vscode/vsce package
code --install-extension lintdec-0.1.1.vsix
```

Set up a provider first with `lintdec provider add`. If the binary lives elsewhere, set `lintdec.path`.
