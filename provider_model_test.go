package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

// A provider whose /models answered must end with a saved and active model,
// otherwise lint has nothing to call.
func TestProviderAddPersistsPickedModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "m-1"}, {"id": "m-2"}},
		})
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	orig := stdinReader
	defer func() { stdinReader = orig }()
	// provider local, name, model type, base URL, api key, test?, set active?, pick #2
	stdinReader = bufio.NewReader(strings.NewReader("4\nclef\n\n" + srv.URL + "\n\nyes\nyes\n2\n"))

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if code := providerAdd(&cfg, nil); code != 0 {
		t.Fatalf("providerAdd exit %d", code)
	}

	b, err := os.ReadFile(filepath.Join(home, "lintdec", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Active != "clef" || got.ActiveModel != "m-2" {
		t.Errorf("active = %s/%q, want clef/m-2", got.Active, got.ActiveModel)
	}
	if !slices.Contains(got.Providers["clef"].Models, "m-2") {
		t.Errorf("saved models = %v, want m-2 in it", got.Providers["clef"].Models)
	}
}

func TestProviderAddStoresAPIKeyInKeychain(t *testing.T) {
	const secret = "sensitive-api-key"
	if os.Getenv("LINTDEC_KEYCHAIN_HELPER") == "1" {
		keyring.MockInit()
		input := "2\nwork\n\n" + secret + "\nno\nyes\n"
		stdinReader = bufio.NewReader(strings.NewReader(input))
		cfg := Config{Providers: map[string]Provider{}}
		if code := providerAdd(&cfg, nil); code != 0 {
			t.Fatalf("providerAdd exit %d", code)
		}
		p := cfg.Providers["work"]
		if p.APIKey != "" || p.APIKeyRef == "" {
			t.Fatalf("key not protected: %+v", p)
		}
		if value, err := keyring.Get(apiKeyKeychainService, p.APIKeyRef); err != nil || value != secret {
			t.Fatalf("keychain lookup failed: %v", err)
		}
		data, err := os.ReadFile(configPath())
		if err != nil || strings.Contains(string(data), secret) {
			t.Fatalf("key leaked to config: %v", err)
		}
		source, err := providerAuth(p)
		if err != nil {
			t.Fatal(err)
		}
		if value, err := source.token(context.Background()); err != nil || value != secret {
			t.Fatalf("provider key lookup: %v", err)
		}
		return
	}
	for _, accessible := range []string{"1", "0"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProviderAddStoresAPIKeyInKeychain$")
		cmd.Env = append(os.Environ(), "LINTDEC_KEYCHAIN_HELPER=1", "LINTDEC_ACCESSIBLE="+accessible, "XDG_CONFIG_HOME="+t.TempDir())
		if output, err := cmd.CombinedOutput(); err != nil || strings.Contains(string(output), secret) {
			t.Fatalf("keychain setup (accessible=%s) failed: %v, output=%s", accessible, err, output)
		}
	}
}

func TestProviderAddSupportsGoingBackBeforeSaving(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LINTDEC_ACCESSIBLE", "1")
	orig := stdinReader
	defer func() { stdinReader = orig }()
	stdinReader = bufio.NewReader(strings.NewReader("4\nwork\n1\nback\nback\nback\n1\n\n2\n2\n4\n\n/token\npolicy\nno\nyes\n"))
	cfg := Config{Providers: map[string]Provider{}}
	if code := providerAdd(&cfg, nil); code != 0 {
		t.Fatalf("setup exited %d", code)
	}
	p := cfg.Providers["work"]
	if p.Protocol != "systemone" || p.Auth != "openrouter-oidc" || p.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("back navigation lost changes: %+v", p)
	}
}

func TestProviderWizardSavesOIDCWithoutToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("OPENROUTER_OIDC_TOKEN_FILE", "")
	t.Setenv("OPENROUTER_FEDERATION_POLICY_ID", "")
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("sensitive-subject-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := stdinReader
	defer func() { stdinReader = orig }()
	stdinReader = bufio.NewReader(strings.NewReader("1\njev\n2\n2\n4\n\n" + file + "\npolicy\nno\nyes\n"))
	cfg := Config{Providers: map[string]Provider{}}
	if code := providerAdd(&cfg, nil); code != 0 {
		t.Fatalf("providerAdd exit %d", code)
	}
	p := cfg.Providers["jev"]
	if p.Auth != "openrouter-oidc" || p.OIDCTokenFile != file || p.FederationPolicyID != "policy" || p.APIKey != "" || p.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("unexpected provider config: %+v", p)
	}
	data, err := os.ReadFile(filepath.Join(home, "lintdec", "config.json"))
	if err != nil || strings.Contains(string(data), "sensitive-subject-token") {
		t.Fatalf("token persisted or config unreadable: %v", err)
	}
}

func TestModelListShowsModellessProvider(t *testing.T) {
	c := Config{Providers: map[string]Provider{
		"clef": {BaseURL: "http://127.0.0.1:11436/v1"},
	}}
	out := captureStdout(t, func() { modelList(c) })
	if !strings.Contains(out, "clef") || !strings.Contains(out, "no models yet") {
		t.Errorf("model list hid the provider: %q", out)
	}
}

func TestProviderTestDoesNotMakeInferenceRequest(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if err := testConnection(Provider{BaseURL: srv.URL, Models: []string{"model"}}, "model"); err == nil || posts != 0 {
		t.Fatalf("expected discovery error without inference: err=%v posts=%d", err, posts)
	}
}

func TestSystemOneHealthNonJSONHintsOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "OK")
	}))
	defer srv.Close()

	_, err := fetchSystemOneModel(Provider{Protocol: "systemone", BaseURL: srv.URL + "/v1"})
	if err == nil || !strings.Contains(err.Error(), "protocol openai") {
		t.Fatalf("err = %v, want a hint to use protocol openai", err)
	}
}

func TestProviderTestOnlyDiscoversModels(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, body string
		status               int
		wantError            bool
	}{
		{"chat success", "", `{"data":[{"id":"model"}]}`, http.StatusOK, false},
		{"chat empty", "", `{"data":[]}`, http.StatusOK, true},
		{"chat malformed", "", `{`, http.StatusOK, true},
		{"systemone health", "systemone", `{"model":"model"}`, http.StatusOK, false},
		{"systemone failure", "systemone", `unavailable`, http.StatusServiceUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			err := testConnection(Provider{BaseURL: srv.URL, Protocol: tc.protocol, Models: []string{"model"}}, "model")
			wantPath := "GET /models"
			if tc.protocol == "systemone" {
				wantPath = "GET /health"
			}
			if (err != nil) != tc.wantError || !slices.Equal(requests, []string{wantPath}) {
				t.Fatalf("err=%v requests=%v", err, requests)
			}
		})
	}
}

func TestProviderDiscoveryRejectsCredentialRedirect(t *testing.T) {
	for _, protocol := range []string{"", "systemone"} {
		t.Run(protocol, func(t *testing.T) {
			var forwarded bool
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded = true
			}))
			defer target.Close()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("missing credential")
				}
				w.Header().Set("Location", target.URL)
				w.WriteHeader(http.StatusTemporaryRedirect)
				io.WriteString(w, `{"data":[{"id":"secret"}],"model":"secret"}`)
			}))
			defer srv.Close()
			_, err := fetchModels(Provider{BaseURL: srv.URL, Protocol: protocol, APIKey: "secret"})
			if err == nil || !strings.Contains(err.Error(), "HTTP 307") || strings.Contains(err.Error(), "secret") || forwarded {
				t.Fatalf("err=%v forwarded=%v", err, forwarded)
			}
		})
	}
}

func TestOpenRouterSystemOneDiscoveryRejectsUntrustedEndpoints(t *testing.T) {
	for _, base := range []string{
		"http://openrouter.ai/api/v1",
		"https://openrouter.ai:443/api/v1",
		"https://user@openrouter.ai/api/v1",
		"https://openrouter.ai/apiary/v1",
		"https://openrouter.ai/api/v1?secret=x",
		"https://openrouter.ai/api/../other",
	} {
		_, err := fetchSystemOneModel(Provider{Protocol: "systemone", BaseURL: base, APIKey: "secret"})
		if err == nil || !strings.Contains(err.Error(), "trusted HTTPS") || strings.Contains(err.Error(), "secret") {
			t.Errorf("base=%q err=%v", base, err)
		}
	}
}

func TestProviderDiscoveryRedactsAPIKeyFromErrors(t *testing.T) {
	for _, protocol := range []string{"", "systemone"} {
		t.Run(protocol, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, r.Header.Get("Authorization"))
			}))
			defer srv.Close()
			_, err := fetchModels(Provider{BaseURL: srv.URL, Protocol: protocol, APIKey: "sensitive-key"})
			if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "sensitive-key") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}
