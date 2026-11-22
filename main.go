package main

import (
	"fmt"
	"os"
)

// Set by CI with -ldflags "-X main.version=<sha>"; the release job pins each build by commit.
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "provider" || args[0] == "providers") {
		os.Exit(providerCmd(args[1:]))
	}
	if len(args) > 0 && (args[0] == "model" || args[0] == "models") {
		os.Exit(modelCmd(args[1:]))
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
		return
	case "version", "-v", "--version":
		fmt.Println("lintdec " + version)
		return
	case "update":
		os.Exit(updateCmd())
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
  lintdec provider [list|add|use|rm|test] ...
  lintdec model [provider|list|use|add|rm] ...
  lintdec update                   download and install the latest release
  lintdec --list-rules             list rule ids
  lintdec -h                       this help

Lint flags:
  --rules a,b,c         only these rules (default: all)
  --include g1,g2       glob include filters (default: *.go,*.md)
  --exclude g1,g2       glob exclude filters
  --severity low        report findings at this severity or higher: low|med|high
  --out text            output: text|json|jsonl
  --provider name       provider name from the global config
  --base-url URL        API base URL (overrides provider)
  --protocol PROTO      provider protocol: openai|systemone
  --model ID            model id (overrides provider)
  --api-key KEY         API key (overrides provider)
  --workers N           parallel model calls (default 8)
  --budget N            max model calls this run, 0 = unlimited
  --temperature 0       sampling temperature
  --threshold 0.75      noul probability threshold for systemone providers
  --no-cache            bypass ~/.cache/lintdec/cache
  --no-ignore           report findings despite inline //lintdec:ignore
  --ledger FILE         append findings as JSONL to FILE as they are decided
  --retry-errors        re-run only the checks that errored last run
  --dump-units          print split units and exit (no model calls)
  --quiet               no progress output

Config: flags > env (LINTDEC_*) > lintdec.yaml (repo) > ~/.config/lintdec/config.json
Inline suppression: //lintdec:ignore rule-id (applies to the next finding within 2 lines)
Exit codes: 0 clean, 1 findings, 2 error
`)
}
