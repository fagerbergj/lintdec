package main

import (
	"fmt"
	"os"
	"strings"
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
	if len(args) > 0 && args[0] == "doctor" {
		os.Exit(doctorCmd(args[1:]))
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
	case "--list-rules":
		listRules()
		return
	}
	os.Exit(lintCmd(args))
}

func listRules() {
	interactive := terminal(os.Stdout)
	if interactive {
		fmt.Printf("\n  %s\n\n", paint("RULES", bold, colorEnabled(true)))
	}
	for _, r := range rules {
		state := "on"
		if defaultOff[r.ID] {
			state = "off" // still runs when named with --rules or on: in lintdec.yaml
		}
		if interactive {
			fmt.Printf("  %s  %-3s  %s\n  %s       %s\n", paint(padText(r.ID, 26), accent, colorEnabled(true)), state, cleanText(r.Summary), padText("", 26), paint(cleanText(r.Source), dim, colorEnabled(true)))
		} else {
			fmt.Printf("%-26s %-3s %s\t%s\n", r.ID, state, r.Summary, r.Source)
		}
	}
}

func usage() {
	text := `lintdec — decision-model code and documentation linter

Usage:
  lintdec [flags] <path>...        lint files, dirs, or glob patterns (default: .)
  lintdec doctor [flags] [path]    audit deterministic linters & CI (--fix with a chat LLM)
  lintdec provider                 choose or add a provider
  lintdec provider [list|add|use|rm|test] ...
  lintdec model                    choose a model
  lintdec model [provider|list|use|add|rm] ...
  lintdec --list-rules             list rule ids
  lintdec -h                       this help

Lint flags:
  --rules a,b,c         only these rules (default: all)
  --include g1,g2       glob include filters (default: supported code and Markdown)
  --exclude g1,g2       glob exclude filters
  --diff REF            lint units changed since the merge base with REF
  --severity info       report findings at this level or higher: info|warning|error
  --out text            output: text|json|jsonl
  --provider name       provider name from the global config
  --base-url URL        API base URL (overrides provider)
  --protocol PROTO      provider protocol: openai|systemone
  --model ID            model id (overrides provider)
  --api-key KEY         API key (overrides provider)
  --workers N           parallel model calls (default 8)
  --budget N            max model calls this run, 0 = unlimited
  --temperature 0       sampling temperature
  --threshold 0.5       minimum confidence to report (overrides per-rule floors, keeps vetoes)
  --no-cache            bypass ~/.cache/lintdec/cache
  --no-ignore           report findings despite inline //lintdec:ignore
  --ledger FILE         append findings as JSONL to FILE as they are decided
  --retry-errors        re-run only the checks that errored last run
  --dump-units          print split units and exit (no model calls)
  --quiet               no progress output

Config: flags > env (LINTDEC_*) > lintdec.yaml (repo) > ~/.config/lintdec/config.json
Inline suppression: //lintdec:ignore rule-id (applies to the next finding within 2 lines)
Exit codes: 0 clean, 1 findings, 2 error or incomplete run
`
	if !terminal(os.Stderr) {
		fmt.Fprint(os.Stderr, text)
		return
	}
	color := colorEnabled(true)
	for _, line := range strings.SplitAfter(text, "\n") {
		switch {
		case strings.HasPrefix(line, "lintdec —"):
			fmt.Fprint(os.Stderr, paint(line, accent, color))
		case line == "Usage:\n" || line == "Lint flags:\n":
			fmt.Fprint(os.Stderr, paint(line, bold, color))
		default:
			fmt.Fprint(os.Stderr, line)
		}
	}
}
