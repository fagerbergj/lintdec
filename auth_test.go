package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenRouterOIDCExchangesOnceAndRefreshesFromFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(file, []byte("one.jwt.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("unexpected exchange request")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || r.Form.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:jwt" || r.Form.Get("federation_policy_id") != "policy" || r.Form.Get("scope") != "inference" {
			t.Error("incorrect exchange fields")
		}
		n := calls.Add(1)
		want := "one.jwt.token"
		if n > 1 {
			want = "two.jwt.token"
		}
		if r.Form.Get("subject_token") != want {
			t.Error("subject token was not refreshed")
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 900, "scope": "inference"})
	}))
	defer srv.Close()
	source := newOIDCSource(file, "policy", srv.URL, srv.Client())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := source.token(context.Background())
			if err != nil || token != "access" {
				t.Errorf("token: %q, %v", token, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("expected one cached exchange, got %d", calls.Load())
	}
	if err := os.WriteFile(file, []byte("two.jwt.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.expires = time.Now().Add(-time.Second)
	if _, err := source.token(context.Background()); err != nil || calls.Load() != 2 {
		t.Fatalf("refresh: %v, calls=%d", err, calls.Load())
	}
}

func TestOIDCDoesNotLeakSubjectTokenOnError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(file, []byte("sensitive.jwt.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("sensitive.jwt.token"))
	}))
	defer srv.Close()
	_, err := newOIDCSource(file, "policy", srv.URL, srv.Client()).token(context.Background())
	if err == nil || strings.Contains(err.Error(), "sensitive.jwt.token") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestOIDCRejectsMissingIssuedTokenType(t *testing.T) {
	file := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(file, []byte("safe.jwt.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 900})
	}))
	defer srv.Close()
	if _, err := newOIDCSource(file, "policy", srv.URL, srv.Client()).token(context.Background()); err == nil {
		t.Fatal("unscoped token was accepted")
	}
}

func TestOIDCAcceptsOmittedScopeWhenRequested(t *testing.T) {
	file := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(file, []byte("safe.jwt.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 900})
	}))
	defer srv.Close()
	if token, err := newOIDCSource(file, "policy", srv.URL, srv.Client()).token(context.Background()); err != nil || token != "access" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestOIDCShortTokensAreCoalesced(t *testing.T) {
	file := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(file, []byte("safe.jwt.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 30, "scope": "inference"})
	}))
	defer srv.Close()
	source := newOIDCSource(file, "policy", srv.URL, srv.Client())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := source.token(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("expected one exchange; got %d", calls.Load())
	}
}

func TestOIDCExchangeDoesNotFollowRedirect(t *testing.T) {
	file := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(file, []byte("sensitive.jwt.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := newOIDCSource(file, "policy", srv.URL, srv.Client()).token(context.Background())
	if err == nil || received.Load() != 0 {
		t.Fatalf("redirect leaked credentials or unexpectedly succeeded: %v", err)
	}
}

func TestOIDCSystemOneEndToEnd(t *testing.T) {
	file := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(file, []byte("safe.jwt.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	var exchanges, decisions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			exchanges.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 900, "scope": "inference"})
		case "/api/v1/systemone":
			decisions.Add(1)
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Error("missing exchanged token")
			}
			json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"todo-no-owner": map[string]any{"noul": 0.93}}, "usage": map[string]any{"input_tokens": 42, "output_tokens": 2, "cost": 0.00003}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var totals costAccumulator
	rule, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL + "/api", protocol: "systemone", model: "typesafe/jev-1.14", httpc: srv.Client(), source: newOIDCSource(file, "policy", srv.URL+"/oauth/token", srv.Client()), costs: &totals, trustCost: true}
	got, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}})
	if err != nil || !got["todo-no-owner"].Lint || exchanges.Load() != 1 || decisions.Load() != 1 || totals.snapshot().knownUSD != 0.00003 {
		t.Fatalf("unexpected end-to-end result: %v %v %d %d %+v", got, err, exchanges.Load(), decisions.Load(), totals.snapshot())
	}
}

func TestDynamicAuthRejectsUntrustedEndpointsAndMixedSecrets(t *testing.T) {
	t.Setenv("OPENROUTER_OIDC_TOKEN_FILE", "")
	t.Setenv("OPENROUTER_FEDERATION_POLICY_ID", "")
	for _, provider := range []Provider{
		{Auth: "openrouter-oidc", BaseURL: "https://evil.example/api/v1", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai/api/v1", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy", APIKey: "do-not-use"},
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai/apiary/v1", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai/api/../other", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai/api/%2e%2e/other", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai:/api/v1", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai/api/v1?secret=x", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai/api/v1#fragment", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai/api/v1?", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
		{Auth: "openrouter-oidc", BaseURL: "https://user@openrouter.ai/api/v1", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai:443/api/v1", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
	} {
		if _, err := providerAuth(provider); err == nil {
			t.Errorf("accepted unsafe authentication config: %+v", provider)
		}
	}
}

type testAuthSource struct{ calls atomic.Int32 }

func (s *testAuthSource) token(context.Context) (string, error) {
	s.calls.Add(1)
	return "temporary-token", nil
}

func TestAuthorizedDoRefreshesDynamicCredentialsAndDoesNotFollowRedirects(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer temporary-token" {
			t.Error("missing temporary token")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	source := &testAuthSource{}
	for range 2 {
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := authorizedDo(srv.Client(), req, source, "")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("unexpected status: %d", res.StatusCode)
		}
	}
	if source.calls.Load() != 2 || forwarded.Load() != 0 {
		t.Fatal("dynamic tokens were not refreshed or redirect was followed")
	}
}

func TestLintClientUsesDynamicAuthForEachRequest(t *testing.T) {
	source := &testAuthSource{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer temporary-token" {
			t.Error("missing temporary token")
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"todo-no-owner":{"lint":false}}`}}}})
	}))
	defer srv.Close()
	rule, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, model: "model", httpc: srv.Client(), source: source}
	for range 2 {
		if _, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}}); err != nil {
			t.Fatal(err)
		}
	}
	if source.calls.Load() != 2 {
		t.Fatalf("token source called %d times", source.calls.Load())
	}
}

type failingAuthSource struct{}

func (failingAuthSource) token(context.Context) (string, error) {
	return "", fmt.Errorf("sign-in required")
}

func TestAuthenticationFailureIsNotCountedAsInference(t *testing.T) {
	var totals costAccumulator
	rule, _ := findRule("todo-no-owner")
	cl := &client{base: "http://127.0.0.1", model: "model", httpc: &http.Client{}, source: failingAuthSource{}, costs: &totals}
	_, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}})
	if err == nil || totals.snapshot().requests != 0 {
		t.Fatalf("err=%v totals=%+v", err, totals.snapshot())
	}
}

func TestStaticInferenceCredentialDoesNotFollowRedirect(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		w.Write([]byte(r.Header.Get("Authorization")))
	}))
	defer srv.Close()
	rule, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, model: "model", httpc: srv.Client(), apiKey: "sensitive-key"}
	_, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") || strings.Contains(err.Error(), "sensitive-key") || forwarded.Load() != 0 {
		t.Fatalf("unsafe redirect: err=%v forwarded=%d", err, forwarded.Load())
	}
}

func TestDynamicAuthErrorDoesNotLeakCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(r.Header.Get("Authorization")))
	}))
	defer srv.Close()
	rule, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, model: "model", httpc: srv.Client(), source: &testAuthSource{}}
	_, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}})
	if err == nil || strings.Contains(err.Error(), "temporary-token") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestOIDCWaiterCancellationDoesNotCancelRefresh(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("subject"), 0o600); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 900})
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	source := newOIDCSource(file, "policy", srv.URL, srv.Client())
	leader := make(chan error, 1)
	go func() { _, err := source.token(context.Background()); leader <- err }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.token(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled waiter: %v", err)
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	if token, err := source.token(context.Background()); err != nil || token != "access" || calls.Load() != 1 {
		t.Fatalf("cached token=%q err=%v exchanges=%d", token, err, calls.Load())
	}
}

func TestOIDCCanceledRefreshCanBeRetried(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("subject"), 0o600); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "expires_in": 900})
	}))
	defer srv.Close()
	defer close(release)
	source := newOIDCSource(file, "policy", srv.URL, srv.Client())
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := source.token(ctx); first <- err }()
	<-started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh: %v", err)
	}
	if token, err := source.token(context.Background()); err != nil || token != "access" || calls.Load() != 2 {
		t.Fatalf("retry token=%q err=%v exchanges=%d", token, err, calls.Load())
	}
}

func TestKeychainAPIKeyBindsCredentialToConfiguredDestination(t *testing.T) {
	p := Provider{BaseURL: "http://127.0.0.1:8000/v1", APIKeyBaseURL: "http://127.0.0.1:8000/v1", APIKeyRef: "opaque"}
	if source, err := providerAuth(p); err != nil || source == nil {
		t.Fatalf("valid keychain config: %T %v", source, err)
	}
	p.BaseURL = "https://attacker.example/v1"
	if _, err := providerAuth(p); err == nil {
		t.Fatal("accepted keychain destination override")
	}
}

func TestBrowserOIDCBindsCredentialToConfiguredDestination(t *testing.T) {
	p := Provider{Auth: "oidc", BaseURL: "https://example.com:8443/v1", OIDCBaseURL: "https://example.com:8443/v1", OIDCIssuerURL: "https://issuer.example.com", OIDCClientID: "public-client", OIDCResource: "urn:example:api", OIDCScopes: []string{"offline_access"}}
	if source, err := providerAuth(p); err != nil || source == nil {
		t.Fatalf("valid OIDC config: %T %v", source, err)
	}
	p.BaseURL = "https://attacker.example/v1"
	if _, err := providerAuth(p); err == nil {
		t.Fatal("accepted OIDC destination override")
	}
	p.BaseURL, p.OIDCBaseURL = "http://example.com/v1", "http://example.com/v1"
	if _, err := providerAuth(p); err == nil {
		t.Fatal("accepted unencrypted OIDC destination")
	}
}

func TestDynamicAuthAcceptsTrustedEndpoints(t *testing.T) {
	for _, p := range []Provider{
		{Auth: "openrouter-oidc", BaseURL: "https://openrouter.ai/api/v1", OIDCTokenFile: "/tmp/token", FederationPolicyID: "policy"},
	} {
		if source, err := providerAuth(p); err != nil || source == nil {
			t.Errorf("provider %q: source=%T err=%v", p.BaseURL, source, err)
		}
	}
}
