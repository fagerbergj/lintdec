package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

func TestConfigMigratesLegacyModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p := filepath.Join(dir, "lintdec", "config.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"active":"clef","providers":{"clef":{"baseUrl":"http://127.0.0.1:11436/v1","model":"qwen3.8-27b"}}}`), 0o600)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Providers["clef"].Models, []string{"qwen3.8-27b"}) || cfg.Providers["clef"].Model != "" {
		t.Errorf("legacy model not migrated: %+v", cfg.Providers["clef"])
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

func TestSelfUpdateVerifiesChecksumAndReplaces(t *testing.T) {
	payload := []byte("#!/bin/sh\necho new-binary\n")
	sum := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(sum[:])

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]string{
					{"name": "lintdec_linux_amd64", "browser_download_url": srv.URL + "/bin"},
					{"name": "checksums.txt", "browser_download_url": srv.URL + "/sums"},
				},
			})
		case "/sums":
			w.Write([]byte(wantHex + "  lintdec_linux_amd64\n"))
		case "/bin":
			w.Write(payload)
		}
	}))
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "lintdec")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := selfUpdate(exe, srv.URL+"/latest", "linux", "amd64"); err != nil {
		t.Fatalf("selfUpdate failed: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(payload) {
		t.Errorf("binary not replaced: got %q", got)
	}
}

func TestProgressSeverityTotals(t *testing.T) {
	p := newProgress(10, true)
	for range 10 {
		p.addFinding(Finding{Severity: "high"})
	}
	if p.ledgerN != 10 || p.bySev["high"] != 10 {
		t.Errorf("ledgerN=%d high=%d, want 10/10", p.ledgerN, p.bySev["high"])
	}
	if len(p.ledger) != 8 {
		t.Errorf("panel ring len=%d, want 8", len(p.ledger))
	}
	for range 12 {
		p.tick(0, true, "x")
	}
}
