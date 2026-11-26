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
}
