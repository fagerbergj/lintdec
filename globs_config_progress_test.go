package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMatchGlobBraces(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"*.{go,md}", "main.go", true},
		{"*.{go,md}", "README.md", true},
		{"*.{go,md}", "app.ts", false},
		{"pkg/**/*.{ts,tsx}", "pkg/a/b/view.tsx", true},
		{"pkg/**/*.{ts,tsx}", "pkg/a/b/view.js", false},
		{"**/fixtures/**", "a/fixtures/x.go", true},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pat, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pat, tc.path, got, tc.want)
		}
	}
}

func TestAskReturnsDefault(t *testing.T) {
	orig := stdinReader
	defer func() { stdinReader = orig }()

	stdinReader = bufio.NewReader(strings.NewReader("\ncustom\n"))
	var a, b string
	if got := ask(&a, "name", "local"); got != "local" || a != "local" {
		t.Errorf("blank input: got (%q, %q), want (local, local)", got, a)
	}
	if got := ask(&b, "name", "local"); got != "custom" || b != "custom" {
		t.Errorf("typed input: got (%q, %q), want (custom, custom)", got, b)
	}
}

func TestConnectionRejectsMalformedURL(t *testing.T) {
	err := testConnection(Provider{BaseURL: "http://bad host/v1", Models: []string{"m"}}, "m")
	if err == nil {
		t.Fatal("expected error for malformed base URL")
	}
}

func TestModelPickerAndProviderResolution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("LINTDEC_PROVIDER", "")
	t.Setenv("LINTDEC_MODEL", "")
	t.Setenv("LINTDEC_BASE_URL", "")
	t.Setenv("LINTDEC_API_KEY", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"id": "anthropic/claude-sonnet-4"},
				{"id": "google/gemini-2.5-pro"},
			},
		})
	}))
	defer srv.Close()

	cfg := Config{
		Active:      "clef",
		ActiveModel: "qwen3.8-27b",
		Providers: map[string]Provider{
			"clef":       {BaseURL: "http://127.0.0.1:11436/v1", Models: []string{"qwen3.8-27b"}},
			"openrouter": {BaseURL: srv.URL, APIKey: "sk-test"},
		},
	}
	if err := cfg.save(); err != nil {
		t.Fatal(err)
	}

	orig := stdinReader
	defer func() { stdinReader = orig }()
	stdinReader = bufio.NewReader(strings.NewReader("2\n"))
	if code := modelCmd([]string{"openrouter"}); code != 0 {
		t.Fatalf("model openrouter exited %d", code)
	}

	ep, err := resolveProvider(lintOpts{}, ProjectConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if ep.BaseURL != srv.URL || ep.Model != "google/gemini-2.5-pro" || ep.APIKey != "sk-test" {
		t.Errorf("active endpoint = %+v", ep)
	}

	// Passing --model for a model owned by another provider auto-selects that provider.
	ep2, err := resolveProvider(lintOpts{model: "qwen3.8-27b"}, ProjectConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if ep2.BaseURL != "http://127.0.0.1:11436/v1" || ep2.Model != "qwen3.8-27b" {
		t.Errorf("--model auto-provider endpoint = %+v", ep2)
	}
}

func TestProgressSeverityTotals(t *testing.T) {
	p := newProgress(10, true)
	for range 10 {
		p.addFinding(Finding{Severity: "error"})
	}
	if p.ledgerN != 10 || p.bySev["error"] != 10 {
		t.Errorf("ledgerN=%d high=%d, want 10/10", p.ledgerN, p.bySev["error"])
	}
	if len(p.ledger) != 8 {
		t.Errorf("panel ring len=%d, want 8", len(p.ledger))
	}
	for range 12 {
		p.tick(0, true, "x")
	}
	// quiet mode counts checks but never fires milestone lines
	if p.done != 12 || p.milestone != 0 {
		t.Errorf("done=%d milestone=%d, want 12/0", p.done, p.milestone)
	}

	p2 := newProgress(100, true)
	p2.start = time.Now().Add(-10 * time.Second)
	for range 20 {
		p2.tick(3*time.Second, false, "ok") // 6 parallel workers: 20 done in 10s = 2/s
	}
	if r := p2.callsPerSec(); r < 1.5 || r > 2.5 {
		t.Errorf("callsPerSec = %.2f, want ~2.0 (wall-clock aggregate)", r)
	}
}
