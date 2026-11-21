package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "provider" || args[0] == "providers") {
		os.Exit(providerCmd(args[1:]))
	}
	if len(args) > 0 && args[0] == "lint" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(lintCmd(nil))
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage()
	case "version", "-v", "--version":
		fmt.Println("lintdec 0.1.0")
		return
	case "--list-rules":
		for _, r := range rules {
			fmt.Printf("%-24s %s\n", r.ID, r.Summary)
		}
		return
	}
	os.Exit(lintCmd(args))
}

func usage() {
	fmt.Fprint(os.Stderr, `lintdec — decision-model code linter

Usage:
  lintdec [flags] <path>...        lint files, dirs, or glob patterns (default: .)
  lintdec provider [add|use|rm|test] [name]
  lintdec --list-rules             list rule ids
  lintdec -h                       this help

Lint flags:
  --rules a,b,c         only these rules (default: all)
  --include g1,g2       glob include filters (default: *.go,*.md)
  --exclude g1,g2       glob exclude filters
  --severity low        report findings at this severity or higher: low|med|high
  --out text            output: text|json|jsonl
  --provider name       provider name from the global config
  --base-url URL        OpenAI-compatible base URL (overrides provider)
  --model ID            model id (overrides provider)
  --api-key KEY         API key (overrides provider)
  --workers N           parallel model calls (default 4)
  --budget N            max model calls this run, 0 = unlimited
  --temperature 0       sampling temperature
  --no-cache            bypass .lintdec/cache
  --no-ignore           report findings despite inline //lintdec:ignore
  --ledger FILE         append findings as JSONL to FILE as they are decided
  --retry-errors        re-run only the checks that errored last run
  --quiet               no progress output

Config: flags > env (LINTDEC_*) > lintdec.yaml (repo) > ~/.config/lintdec/config.json
Inline suppression: //lintdec:ignore rule-id (applies to the next finding within 2 lines)
Exit codes: 0 clean, 1 findings, 2 error
`)
}
