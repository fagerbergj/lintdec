package main

import (
	"strings"
	"testing"
)

func TestRuleEligibleDeterministicFilters(t *testing.T) {
	longSingleLine := "// " + strings.Repeat("word ", 50)
	cases := []struct {
		rule string
		unit Unit
		want bool
	}{
		{"todo-no-owner", Unit{Kind: "Comment", Text: "// TODO: fix this"}, true},
		{"todo-no-owner", Unit{Kind: "Comment", Text: "// TODO."}, true},
		{"todo-no-owner", Unit{Kind: "Comment", Text: "// TODO! revisit"}, true},
		{"todo-no-owner", Unit{Kind: "Comment", Text: "// TODO, fix later"}, true},
		{"todo-no-owner", Unit{Kind: "Comment", Text: "// (fixme) later"}, true},
		{"todo-no-owner", Unit{Kind: "Comment", Text: "// FIXME(ENG-123) tighten"}, true},
		{"todo-no-owner", Unit{Kind: "Comment", Text: "// Omit todo-no-owner on first call."}, false},
		{"todo-no-owner", Unit{Kind: "Comment", Text: "// no-todo marker and todolist."}, false},
		{"todo-no-owner", Unit{Kind: "Comment", Text: "// Normal explanation comment."}, false},

		{"comment-long", Unit{Kind: "Comment", Start: 1, End: 2, Text: "// one\n// two"}, false},
		{"comment-long", Unit{Kind: "Comment", Start: 1, End: 3, Text: "// one\n// two\n// three"}, true},
		{"comment-long", Unit{Kind: "Comment", Start: 1, End: 1, Text: longSingleLine}, true},

		{"md-topic-sentence", Unit{Kind: "Paragraph", Text: "First sentence here. Second sentence here."}, false},
		{"md-topic-sentence", Unit{Kind: "Paragraph", Text: "First sentence here. Second sentence here. Third sentence here."}, true},

		{"md-prose-list", Unit{Kind: "Paragraph", Text: "A single clear sentence with no list."}, false},
		{"md-prose-list", Unit{Kind: "Paragraph", Text: "Items are alpha, beta, and gamma."}, true},
		{"md-prose-list", Unit{Kind: "Paragraph", Text: "Items are alpha, beta and gamma."}, true},
		{"md-prose-list", Unit{Kind: "Paragraph", Text: "First do X; then do Y; finally do Z."}, true},
		{"md-prose-list", Unit{Kind: "Paragraph", Text: "Run make test and make lint and make docs."}, true},

		{"md-no-purpose", Unit{Kind: "Document", Text: "@AGENTS.md\n"}, false},
		{"md-no-purpose", Unit{Kind: "Document", Text: "# Title\n\nFirst paragraph.\n\nSecond paragraph."}, true},

		{"md-blames-reader", Unit{Kind: "Paragraph", Text: "The parser rejects malformed input."}, false},
		{"md-blames-reader", Unit{Kind: "Paragraph", Text: "You passed an invalid flag."}, true},

		{"md-one-sided-alternatives", Unit{Kind: "Document", Text: "# Guide\n\nRun the command."}, false},
		{"md-one-sided-alternatives", Unit{Kind: "Document", Text: "# ADR\n\n## Alternatives considered\n\nSQLite."}, true},

		{"md-adr-no-cost", Unit{Kind: "Document", File: "README.md", Text: "# App\n\n## Install\n\nRun it."}, false},
		{"md-adr-no-cost", Unit{Kind: "Document", File: "docs/adr/001-db.md", Text: "# Use Postgres\n\nBody."}, true},
		{"md-adr-no-cost", Unit{Kind: "Document", File: "docs/001.md", Text: "# Use Postgres\n\n## Status\nAccepted\n\n## Consequences\nFast."}, true},

		{"md-rfc-no-ask", Unit{Kind: "Document", File: "README.md", Text: "# App\n\n## Usage\n\nRun it."}, false},
		{"md-rfc-no-ask", Unit{Kind: "Document", File: "docs/rfcs/004-cache.md", Text: "# Cache\n\nDetails."}, true},
		{"md-rfc-no-ask", Unit{Kind: "Document", File: "docs/cache.md", Text: "# Cache\n\n## Proposal\n\nAdd LRU."}, true},
		{"md-rfc-no-ask", Unit{Kind: "Document", File: "docs/cache.md", Text: "# Cache Eviction\n\n## Summary\n\n## Detailed design\n\nLRU."}, true},

		{"test-ignored-no-reason", Unit{Kind: "TestFunction", Text: "func TestA(t *testing.T) { t.Skip(\"x\") }"}, true},
		{"test-ignored-no-reason", Unit{Kind: "TestFunction", Text: "@Disabled\nfun check() {}"}, true},
		{"test-ignored-no-reason", Unit{Kind: "TestFunction", Text: "func TestExit(t *testing.T) { exit(0) }"}, false},

		{"test-over-mocked", Unit{Kind: "TestFunction", Text: "it('x', () => { const f = vi.fn(); })"}, true},
		{"test-over-mocked", Unit{Kind: "TestFunction", Text: "it('x', () => { expect(1+1).toBe(2); })"}, false},

		{"test-shared-state", Unit{Kind: "TestFunction", Text: "os.WriteFile(\"/tmp/a\", b, 0644)"}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "os.WriteFile(\"testdata/handoff.json\", b, 0644)"}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "fs.writeFileSync(\"./out.log\", data)"}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "fs.writeFile(`./out.log`, data)"}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "process.env.API_KEY = 'x'"}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "process.env[\"FEATURE_X\"] = \"1\""}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "delete process.env.API_KEY"}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "os.environ[\"HOME\"] = \"/tmp/x\""}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "os.environ[\"PATH\"] += \":/opt/bin\""}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "del os.environ[\"HOME\"]"}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "monkeypatch.setenv('A', '1')"}, true},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "const key = process.env.API_KEY"}, false},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "if (process.env.CI === 'true') return;"}, false},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "home = os.environ[\"HOME\"]"}, false},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "os.WriteFile(filepath.Join(t.TempDir(), \"a\"), b, 0644)"}, false},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "fs.writeFile(tmpdir, data)"}, false},
		{"test-shared-state", Unit{Kind: "TestFunction", Text: "if got := add(1, 2); got != 3 { t.Fatal(got) }"}, false},

		{"md-no-remediation", Unit{Kind: "Section", Text: "## Error 503 on startup\n\nLogs error."}, true},
		{"md-no-remediation", Unit{Kind: "Section", Text: "##### Known issues\n\nFails on arm64."}, true},
		{"md-no-remediation", Unit{Kind: "Section", Text: "## Installation\n\nRun npm ci."}, false},

		{"unbounded-retry", Unit{Kind: "Function", Text: "func fetch() { for { resp, err := get(); if err == nil { return } } }"}, true},
		{"unbounded-retry", Unit{Kind: "Function", Text: "function poll() { for (;;) { check(); } }"}, true},
		{"unbounded-retry", Unit{Kind: "Function", Text: "fn poll() { loop { check(); } }"}, true},
		{"unbounded-retry", Unit{Kind: "Function", Text: "function call(maxRetries: number) { return retry(call); }"}, true},
		{"unbounded-retry", Unit{Kind: "Function", Text: "func add(a, b int) int { return a + b }"}, false},

		{"hand-rolled-stdlib", Unit{Kind: "Function", Start: 1, End: 12, Text: "function parseQueryString(raw) {\n  // manual split\n}"}, true},
		{"hand-rolled-stdlib", Unit{Kind: "Function", Start: 1, End: 4, Text: "function parseQueryString(raw) { return new URLSearchParams(raw); }"}, false},
		{"hand-rolled-stdlib", Unit{Kind: "Function", Start: 1, End: 15, Text: "function sumPrices(items) {\n  let n = 0;\n  return n;\n}"}, false},

		{"lint-config-weakened", Unit{Kind: "LintConfig", Text: `{"rules": {"no-unused-vars": "off"}}`}, true},
		{"lint-config-weakened", Unit{Kind: "LintConfig", Text: `{"rules": {"no-unused-vars": 0}}`}, true},
		{"lint-config-weakened", Unit{Kind: "LintConfig", Text: "severity: warning\n"}, true},
		{"lint-config-weakened", Unit{Kind: "LintConfig", Text: "disabled: [gosec]\n"}, true},
		{"lint-config-weakened", Unit{Kind: "LintConfig", Text: `{"rules": {"no-unused-vars": "error"}}`}, false},
	}
	for _, c := range cases {
		r, ok := findRule(c.rule)
		if !ok {
			t.Fatalf("unknown rule %q", c.rule)
		}
		if got := ruleEligible(r, c.unit); got != c.want {
			t.Errorf("%s on %q (%s:%d-%d) = %v, want %v", c.rule, c.unit.Text, c.unit.File, c.unit.Start, c.unit.End, got, c.want)
		}
	}
}

func TestEveryFilterMapsToKnownRule(t *testing.T) {
	for id := range ruleFilters {
		if _, ok := findRule(id); !ok {
			t.Errorf("ruleFilters has entry for unknown rule %q", id)
		}
	}
	for id := range ruleVetoes {
		if _, ok := findRule(id); !ok {
			t.Errorf("ruleVetoes has entry for unknown rule %q", id)
		}
	}
	for id := range ruleFloors {
		if _, ok := findRule(id); !ok {
			t.Errorf("ruleFloors has entry for unknown rule %q", id)
		}
	}
	for id := range defaultOff {
		if _, ok := findRule(id); !ok {
			t.Errorf("defaultOff has entry for unknown rule %q", id)
		}
	}
}

func TestBuildJobsSkipsIneligibleRules(t *testing.T) {
	long, ok := findRule("comment-long")
	if !ok {
		t.Fatal("missing comment-long rule")
	}
	u := Unit{Kind: "Comment", File: "a.go", Start: 3, End: 4, Text: "// one\n// two"}
	if js := buildJobs(u, []Rule{long}, []string{"", "", "// one", "// two"}); js != nil {
		t.Errorf("a 2-line comment must skip comment-long: %+v", js)
	}
	lcRule, _ := findRule("lint-config-weakened")
	retryRule, _ := findRule("unbounded-retry")
	stdlibRule, _ := findRule("hand-rolled-stdlib")
	if js := buildJobs(Unit{Kind: "Function", Start: 1, End: 10, Text: "func add(a, b int) int { return a + b }"}, []Rule{retryRule, stdlibRule}, nil); js != nil {
		t.Errorf("clean function must skip retry and stdlib rules: %+v", js)
	}
	// Oversized LintConfig (>200 lines) must bypass digestFor so deep off/ignore entries stay visible.
	lines := make([]string, 230)
	for i := range lines {
		lines[i] = "  # padding"
	}
	lines[220] = `  "no-unused-vars": "off"`
	big := Unit{Kind: "LintConfig", File: "eslint.config.mjs", Start: 1, End: len(lines), Text: strings.Join(lines, "\n")}
	js := buildJobs(big, []Rule{lcRule}, lines)
	if len(js) != 1 || !strings.Contains(js[0].unit.Text, `"no-unused-vars": "off"`) {
		t.Fatalf("oversized LintConfig must stay raw in buildJobs, got: %+v", js)
	}
}
