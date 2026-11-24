package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
)

type browserMemoryStore struct {
	mu             sync.Mutex
	data           map[string]string
	fail           bool
	failSetAt      int
	failAfterSetAt int
	sets           int
	blockSetAt     int
	setStarted     chan struct{}
	unblockSet     chan struct{}
}

func (s *browserMemoryStore) Get(service, user string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[service+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}
func (s *browserMemoryStore) Set(service, user, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	if s.sets == s.blockSetAt {
		close(s.setStarted)
		<-s.unblockSet
	}
	if s.fail || s.sets == s.failSetAt {
		return errors.New("sensitive keychain error")
	}
	s.data[service+user] = secret
	if s.sets == s.failAfterSetAt {
		return errors.New("sensitive keychain error")
	}
	return nil
}

type browserIssuer struct {
	refreshResponse, refreshContentType, refreshScope                                                                                                    string
	refreshStarted                                                                                                                                       chan struct{}
	pkceMethods                                                                                                                                          []string
	authEndpoint, tokenEndpoint, jwksEndpoint                                                                                                            string
	atHash, lifetime                                                                                                                                     string
	noRefresh, dropRefresh, invalidGrant                                                                                                                 bool
	resource                                                                                                                                             string
	tokenError                                                                                                                                           bool
	t                                                                                                                                                    *testing.T
	server                                                                                                                                               *httptest.Server
	key                                                                                                                                                  *rsa.PrivateKey
	mu                                                                                                                                                   sync.Mutex
	nonce, challenge, refresh                                                                                                                            string
	refreshes                                                                                                                                            int
	wrongIssuer, wrongIDIssuer, wrongNonce, wrongAudience, expired, wrongSignature, wrongState, redirectToken, redirectDiscovery, redirectKeys, badScope bool
	refreshID                                                                                                                                            string
	leaks                                                                                                                                                atomic.Int32
}

func newBrowserIssuer(t *testing.T) *browserIssuer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("OIDC browser login is unavailable on Windows")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &browserIssuer{t: t, key: key, refresh: "refresh-secret-0", pkceMethods: []string{"S256"}}
	m.server = httptest.NewTLSServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}
func (m *browserIssuer) sign(nonce, subject, access string, refresh bool) string {
	key := m.key
	if m.wrongSignature {
		key, _ = rsa.GenerateKey(rand.Reader, 2048)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "key1"))
	if err != nil {
		m.t.Fatal(err)
	}
	issuer := m.server.URL
	if m.wrongIssuer || m.wrongIDIssuer {
		issuer += "/wrong"
	}
	audience := "client-id"
	if m.wrongAudience {
		audience = "wrong-client"
	}
	if m.wrongNonce {
		nonce = "wrong-nonce"
	}
	expiry := time.Now().Add(time.Hour)
	if m.expired {
		expiry = time.Now().Add(-time.Hour)
	}
	claims := map[string]any{"iss": issuer, "sub": subject, "aud": audience, "exp": expiry.Unix(), "iat": time.Now().Unix()}
	switch m.lifetime {
	case "missing iat":
		delete(claims, "iat")
	case "null iat":
		claims["iat"] = nil
	case "string iat":
		claims["iat"] = fmt.Sprint(time.Now().Unix())
	case "future iat":
		claims["iat"] = time.Now().Add(62 * time.Second).Unix()
	case "within skew":
		claims["iat"] = time.Now().Add(time.Minute).Unix()
	case "exp before iat":
		claims["exp"] = time.Now().Add(15 * time.Second).Unix()
		claims["iat"] = time.Now().Add(30 * time.Second).Unix()
	case "exp equal iat":
		claims["iat"] = time.Now().Add(30 * time.Second).Unix()
		claims["exp"] = claims["iat"]
	case "missing exp":
		delete(claims, "exp")
	}
	switch m.atHash {
	case "valid":
		sum := sha256.Sum256([]byte(access))
		claims["at_hash"] = base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2])
	case "wrong":
		claims["at_hash"] = "wrong"
	case "empty":
		claims["at_hash"] = ""
	case "null":
		claims["at_hash"] = nil
	}
	if !refresh || m.refreshID == "nonce" {
		claims["nonce"] = nonce
	} else if m.refreshID == "empty nonce" {
		claims["nonce"] = ""
	} else if m.refreshID == "null nonce" {
		claims["nonce"] = nil
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		m.t.Fatal(err)
	}
	return raw
}
func (m *browserIssuer) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		if m.redirectDiscovery {
			http.Redirect(w, r, m.server.URL+"/leak", http.StatusFound)
			return
		}
		issuer := m.server.URL
		if m.wrongIssuer {
			issuer += "/wrong"
		}
		auth, token, keys := m.authEndpoint, m.tokenEndpoint, m.jwksEndpoint
		if auth == "" {
			auth = m.server.URL + "/authorize"
		}
		if token == "" {
			token = m.server.URL + "/token"
		}
		if keys == "" {
			keys = m.server.URL + "/keys"
		}
		metadata := map[string]any{"issuer": issuer, "authorization_endpoint": auth, "token_endpoint": token, "jwks_uri": keys, "id_token_signing_alg_values_supported": []string{"RS256"}}
		if m.pkceMethods != nil {
			metadata["code_challenge_methods_supported"] = m.pkceMethods
		}
		_ = json.NewEncoder(w).Encode(metadata)
	case "/keys":
		if m.redirectKeys {
			http.Redirect(w, r, m.server.URL+"/leak", http.StatusFound)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &m.key.PublicKey, KeyID: "key1", Algorithm: "RS256", Use: "sig"}}})
	case "/authorize":
		q := r.URL.Query()
		m.mu.Lock()
		m.nonce, m.challenge = q.Get("nonce"), q.Get("code_challenge")
		m.mu.Unlock()
		if q.Get("client_id") != "client-id" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("scope") != "offline_access openid" || q.Get("resource") != m.resource {
			m.t.Errorf("bad authorization request: %v", q)
		}
		callback, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || callback.Hostname() != "127.0.0.1" {
			m.t.Errorf("unsafe callback")
			w.WriteHeader(400)
			return
		}
		state := q.Get("state")
		if m.wrongState {
			state = "wrong-state"
		}
		callback.RawQuery = url.Values{"state": {state}, "code": {"code-secret"}}.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	case "/token":
		if m.tokenError {
			http.Error(w, "code-secret access-secret refresh-secret", http.StatusBadRequest)
			return
		}
		if m.redirectToken {
			http.Redirect(w, r, m.server.URL+"/leak", http.StatusTemporaryRedirect)
			return
		}
		if err := r.ParseForm(); err != nil {
			m.t.Error(err)
			w.WriteHeader(400)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Form.Get("client_id") != "client-id" {
			m.t.Errorf("missing public client ID")
		}
		if r.Form.Get("resource") != m.resource {
			m.t.Errorf("wrong token request resource")
		}
		response := map[string]any{"access_token": "access-secret", "token_type": "Bearer", "expires_in": 3600}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "code-secret" || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != m.challenge {
				m.t.Errorf("bad PKCE exchange")
				w.WriteHeader(400)
				return
			}
			if !m.noRefresh {
				response["refresh_token"] = m.refresh
			}
			response["id_token"] = m.sign(m.nonce, "subject", "access-secret", false)
		case "refresh_token":
			if m.invalidGrant {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			if r.Form.Get("refresh_token") != m.refresh {
				m.t.Errorf("refresh token reused")
				w.WriteHeader(400)
				return
			}
			m.refreshes++
			if m.refreshResponse != "omitted" {
				m.refresh = fmt.Sprintf("refresh-secret-%d", m.refreshes)
			}
			switch m.refreshResponse {
			case "omitted":
			case "empty":
				response["refresh_token"] = ""
			case "null":
				response["refresh_token"] = nil
			case "upper-null":
				response["REFRESH_TOKEN"] = nil
			case "upper-rotated":
				response["REFRESH_TOKEN"] = m.refresh
			default:
				response["refresh_token"] = m.refresh
			}
			response["access_token"] = fmt.Sprintf("access-secret-%d", m.refreshes)
			if m.refreshStarted != nil {
				close(m.refreshStarted)
				<-r.Context().Done()
				return
			}
			if m.dropRefresh {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					m.t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			if m.refreshID == "empty-id" {
				response["id_token"] = ""
			} else if m.refreshID == "null-id" {
				response["id_token"] = nil
			} else if m.refreshID != "" {
				subject := "subject"
				if m.refreshID == "subject" {
					subject = "other"
				}
				response["id_token"] = m.sign(m.nonce, subject, response["access_token"].(string), true)
			}
		default:
			w.WriteHeader(400)
			return
		}
		if m.badScope {
			response["scope"] = "openid"
		} else if r.Form.Get("grant_type") == "refresh_token" && m.refreshScope != "" {
			response["scope"] = m.refreshScope
		}
		if r.Form.Get("grant_type") == "refresh_token" && m.refreshContentType != "" {
			w.Header().Set("Content-Type", m.refreshContentType)
			values := url.Values{}
			for key, value := range response {
				if value == nil {
					values.Set(key, "null")
				} else {
					values.Set(key, fmt.Sprint(value))
				}
			}
			if m.refreshResponse == "duplicate" {
				values.Add("refresh_token", "")
			}
			_, _ = io.WriteString(w, values.Encode())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("grant_type") == "refresh_token" && m.refreshResponse == "duplicate" {
			_, _ = io.WriteString(w, `{"access_token":"access-secret","token_type":"Bearer","expires_in":3600,"refresh_token":"", "refresh_token":"new-token"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	case "/leak":
		m.leaks.Add(1)
		w.WriteHeader(500)
	default:
		w.WriteHeader(404)
	}
}
func browserForTest(t *testing.T, m *browserIssuer, store *browserMemoryStore, dir string, resource ...string) *OIDCBrowser {
	t.Helper()
	requestedResource := ""
	if len(resource) != 0 {
		requestedResource = resource[0]
	}
	b, err := NewOIDCBrowser(OIDCBrowserOptions{Issuer: m.server.URL, ClientID: "client-id", Resource: requestedResource, Scopes: []string{"offline_access"}})
	if err != nil {
		t.Fatal(err)
	}
	b.store = store
	b.lockDir = filepath.Join(dir, "locks")
	b.httpClient.Transport = oidcLimitedTransport{m.server.Client().Transport}
	return b
}
func browserOpen(m *browserIssuer) func(string) error {
	return func(authURL string) error {
		res, err := m.server.Client().Get(authURL)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
}
func newMemoryStore() *browserMemoryStore { return &browserMemoryStore{data: map[string]string{}} }

func TestOIDCBrowserLoginAndRotatingRefresh(t *testing.T) {
	m := newBrowserIssuer(t)
	store := newMemoryStore()
	dir := t.TempDir()
	b := browserForTest(t, m, store, dir)
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatal(err)
	}
	if access, err := b.Token(context.Background()); err != nil || access != "access-secret" {
		t.Fatalf("login token: %q %v", access, err)
	}
	data, err := store.Get("lintdec-oidc", b.key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data, "access-secret") || !strings.Contains(data, "refresh-secret-0") {
		t.Fatalf("bad keychain session")
	}
	b.expiry = time.Now().Add(-time.Second)
	if access, err := b.Token(context.Background()); err != nil || access != "access-secret-1" {
		t.Fatalf("refresh: %q %v", access, err)
	}
	b2 := browserForTest(t, m, store, dir)
	if access, err := b2.Token(context.Background()); err != nil || access != "access-secret-2" {
		t.Fatalf("persisted rotation: %q %v", access, err)
	}
	data, _ = store.Get("lintdec-oidc", b.key)
	if !strings.Contains(data, "refresh-secret-2") || strings.Contains(data, "refresh-secret-1") {
		t.Fatal("refresh rotation not persisted")
	}
	files, err := filepath.Glob(filepath.Join(dir, "locks", "*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("lock files: %v %v", files, err)
	}
	contents, err := os.ReadFile(files[0])
	if err != nil || strings.Contains(string(contents), "secret") {
		t.Fatal("secret in lock file")
	}
}

func TestOIDCBrowserRejectsInvalidLogin(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*browserIssuer)
		deadline  bool
	}{
		{"discovery issuer", func(m *browserIssuer) { m.wrongIssuer = true }, false},
		{"missing S256 metadata", func(m *browserIssuer) { m.pkceMethods = nil }, false},
		{"plain-only PKCE", func(m *browserIssuer) { m.pkceMethods = []string{"plain"} }, false},
		{"foreign authorization endpoint", func(m *browserIssuer) { m.authEndpoint = "https://foreign.example/authorize" }, false},
		{"foreign token endpoint", func(m *browserIssuer) { m.tokenEndpoint = "https://foreign.example/token" }, false},
		{"foreign JWKS endpoint", func(m *browserIssuer) { m.jwksEndpoint = "https://foreign.example/keys" }, false},
		{"reserved auth parameter", func(m *browserIssuer) { m.authEndpoint = m.server.URL + "/authorize?code_challenge=attacker" }, false},
		{"encoded reserved auth parameter", func(m *browserIssuer) { m.authEndpoint = m.server.URL + "/authorize?%63lient_id=attacker" }, false},
		{"duplicate auth parameter", func(m *browserIssuer) { m.authEndpoint = m.server.URL + "/authorize?custom=a&custom=b" }, false},
		{"reserved token parameter", func(m *browserIssuer) { m.tokenEndpoint = m.server.URL + "/token?code_verifier=attacker" }, false},
		{"missing refresh token", func(m *browserIssuer) { m.noRefresh = true }, false},
		{"wrong access hash", func(m *browserIssuer) { m.atHash = "wrong" }, false},
		{"empty access hash", func(m *browserIssuer) { m.atHash = "empty" }, false},
		{"null access hash", func(m *browserIssuer) { m.atHash = "null" }, false},
		{"missing iat", func(m *browserIssuer) { m.lifetime = "missing iat" }, false},
		{"null iat", func(m *browserIssuer) { m.lifetime = "null iat" }, false},
		{"string iat", func(m *browserIssuer) { m.lifetime = "string iat" }, false},
		{"future iat", func(m *browserIssuer) { m.lifetime = "future iat" }, false},
		{"expiry before iat", func(m *browserIssuer) { m.lifetime = "exp before iat" }, false},
		{"expiry equal iat", func(m *browserIssuer) { m.lifetime = "exp equal iat" }, false},
		{"missing exp", func(m *browserIssuer) { m.lifetime = "missing exp" }, false},
		{"ID token issuer", func(m *browserIssuer) { m.wrongIDIssuer = true }, false},
		{"discovery redirect", func(m *browserIssuer) { m.redirectDiscovery = true }, false},
		{"JWKS redirect", func(m *browserIssuer) { m.redirectKeys = true }, false},
		{"state", func(m *browserIssuer) { m.wrongState = true }, true},
		{"nonce", func(m *browserIssuer) { m.wrongNonce = true }, false},
		{"audience", func(m *browserIssuer) { m.wrongAudience = true }, false},
		{"expiry", func(m *browserIssuer) { m.expired = true }, false},
		{"signature", func(m *browserIssuer) { m.wrongSignature = true }, false},
		{"token redirect", func(m *browserIssuer) { m.redirectToken = true }, false},
		{"token error redaction", func(m *browserIssuer) { m.tokenError = true }, false},
		{"missing scope", func(m *browserIssuer) { m.badScope = true }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newBrowserIssuer(t)
			tt.configure(m)
			store := newMemoryStore()
			b := browserForTest(t, m, store, t.TempDir())
			ctx := context.Background()
			if tt.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancel()
			}
			err := b.Login(ctx, browserOpen(m))
			if err == nil {
				t.Fatal("invalid login accepted")
			}
			for _, secret := range []string{"code-secret", "access-secret", "refresh-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("secret exposed in error")
				}
			}
			if m.leaks.Load() != 0 {
				t.Fatal("credentials followed redirect")
			}
			if _, err := store.Get("lintdec-oidc", b.key); !errors.Is(err, keyring.ErrNotFound) {
				t.Fatal("invalid login persisted")
			}
		})
	}
}

func TestOIDCBrowserRefreshVerification(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*browserIssuer)
	}{
		{"valid ID token without nonce", func(m *browserIssuer) { m.refreshID = "valid" }},
		{"valid ID token with nonce", func(m *browserIssuer) { m.refreshID = "nonce" }},
		{"valid access hash", func(m *browserIssuer) { m.refreshID = "valid"; m.atHash = "valid" }},
		{"valid future iat within skew", func(m *browserIssuer) { m.refreshID = "valid"; m.lifetime = "within skew" }},
		{"wrong access hash", func(m *browserIssuer) { m.refreshID = "valid"; m.atHash = "wrong" }},
		{"empty access hash", func(m *browserIssuer) { m.refreshID = "valid"; m.atHash = "empty" }},
		{"null access hash", func(m *browserIssuer) { m.refreshID = "valid"; m.atHash = "null" }},
		{"missing iat", func(m *browserIssuer) { m.refreshID = "valid"; m.lifetime = "missing iat" }},
		{"future iat", func(m *browserIssuer) { m.refreshID = "valid"; m.lifetime = "future iat" }},
		{"expiry before iat", func(m *browserIssuer) { m.refreshID = "valid"; m.lifetime = "exp before iat" }},
		{"wrong subject", func(m *browserIssuer) { m.refreshID = "subject" }},
		{"wrong nonce", func(m *browserIssuer) { m.refreshID = "nonce"; m.wrongNonce = true }},
		{"empty nonce", func(m *browserIssuer) { m.refreshID = "empty nonce" }},
		{"null nonce", func(m *browserIssuer) { m.refreshID = "null nonce" }},
		{"wrong audience", func(m *browserIssuer) { m.refreshID = "valid"; m.wrongAudience = true }},
		{"expired ID token", func(m *browserIssuer) { m.refreshID = "valid"; m.expired = true }},
		{"wrong signature", func(m *browserIssuer) { m.refreshID = "valid"; m.wrongSignature = true }},
		{"missing scope", func(m *browserIssuer) { m.badScope = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newBrowserIssuer(t)
			b := browserForTest(t, m, newMemoryStore(), t.TempDir())
			if err := b.Login(context.Background(), browserOpen(m)); err != nil {
				t.Fatal(err)
			}
			tt.configure(m)
			b.expiry = time.Time{}
			_, err := b.Token(context.Background())
			valid := strings.HasPrefix(tt.name, "valid ")
			if (err == nil) != valid {
				t.Fatalf("unexpected refresh result: %v", err)
			}
		})
	}
}

func TestOIDCBrowserCancellationAndKeychainFailure(t *testing.T) {
	m := newBrowserIssuer(t)
	store := newMemoryStore()
	b := browserForTest(t, m, store, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Login(ctx, browserOpen(m)); !errors.Is(err, context.Canceled) {
		t.Fatalf("login cancellation: %v", err)
	}
	if _, err := b.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("token cancellation: %v", err)
	}
	if _, err := b.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
		t.Fatalf("missing session: %v", err)
	}
	store.fail = true
	err := b.Login(context.Background(), browserOpen(m))
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe keychain failure: %v", err)
	}
	if b.access != "" {
		t.Fatal("access token retained after storage failure")
	}
}

func TestOIDCBrowserSerializesRefreshAcrossInstances(t *testing.T) {
	m := newBrowserIssuer(t)
	store := newMemoryStore()
	dir := t.TempDir()
	b := browserForTest(t, m, store, dir)
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		other := browserForTest(t, m, store, dir)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := other.Token(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m.refreshes != 8 {
		t.Fatalf("refresh count %d", m.refreshes)
	}
}

func TestOIDCBrowserOptions(t *testing.T) {
	for _, opts := range []OIDCBrowserOptions{
		{Issuer: "http://example.com", ClientID: "id"},
		{Issuer: "https://user:password@example.com", ClientID: "id"},
		{Issuer: "https://example.com", ClientID: ""},
		{Issuer: "https://example.com", ClientID: "id", Scopes: []string{"openid offline_access"}},
		{Issuer: "https://example.com", ClientID: "id", Resource: "relative-resource"},
		{Issuer: "https://example.com", ClientID: "id", Resource: "https://example.com/#fragment"},
	} {
		if _, err := NewOIDCBrowser(opts); err == nil {
			t.Fatalf("accepted invalid options")
		}
	}
}

func TestOIDCBrowserLockAcrossProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("OIDC browser login is unavailable on Windows")
	}
	if dir := os.Getenv("LINTDEC_TEST_LOCK_DIR"); dir != "" {
		b := &OIDCBrowser{lockDir: dir, key: os.Getenv("LINTDEC_TEST_LOCK_KEY"), gate: make(chan struct{}, 1)}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		unlock, err := b.acquire(ctx)
		if os.Getenv("LINTDEC_TEST_LOCK_FREE") == "yes" {
			if err != nil {
				t.Fatal(err)
			}
			unlock()
		} else if !errors.Is(err, context.DeadlineExceeded) {
			if unlock != nil {
				unlock()
			}
			t.Fatalf("lock should be held: %v", err)
		}
		return
	}
	b := &OIDCBrowser{lockDir: filepath.Join(t.TempDir(), "locks"), key: "cross-process", gate: make(chan struct{}, 1)}
	unlock, err := b.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	child := func(free string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOIDCBrowserLockAcrossProcesses$")
		cmd.Env = append(os.Environ(), "LINTDEC_TEST_LOCK_DIR="+b.lockDir, "LINTDEC_TEST_LOCK_KEY="+b.key, "LINTDEC_TEST_LOCK_FREE="+free)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child lock test: %v: %s", err, out)
		}
	}
	child("no")
	unlock()
	child("yes")
}

func TestOIDCBrowserScopeChangeRequiresLogin(t *testing.T) {
	m := newBrowserIssuer(t)
	store := newMemoryStore()
	dir := t.TempDir()
	b := browserForTest(t, m, store, dir)
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatal(err)
	}
	other := browserForTest(t, m, store, dir)
	other.scopes = []string{"openid", "different"}
	if _, err := other.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
		t.Fatalf("scope mismatch: %v", err)
	}
	if m.refreshes != 0 {
		t.Fatal("scope mismatch reused refresh token")
	}
}

func TestOIDCBrowserResourceOnLoginAndRefresh(t *testing.T) {
	m := newBrowserIssuer(t)
	m.resource = "https://openrouter.ai/api/v1"
	store := newMemoryStore()
	dir := t.TempDir()
	b := browserForTest(t, m, store, dir, m.resource)
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatal(err)
	}
	b.expiry = time.Time{}
	if access, err := b.Token(context.Background()); err != nil || access != "access-secret-1" {
		t.Fatalf("resource refresh: %q %v", access, err)
	}
	other := browserForTest(t, m, store, dir, "https://other.example/api")
	if other.key == b.key {
		t.Fatal("resources share a keychain record")
	}
	if _, err := other.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
		t.Fatalf("resource mismatch: %v", err)
	}
}

func TestOIDCBrowserCallbackTimeout(t *testing.T) {
	m := newBrowserIssuer(t)
	b := browserForTest(t, m, newMemoryStore(), t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := b.Login(ctx, func(string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("callback timeout: %v", err)
	}
}

func TestOIDCBrowserRejectsAccountChange(t *testing.T) {
	m := newBrowserIssuer(t)
	store := newMemoryStore()
	b := browserForTest(t, m, store, t.TempDir())
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatal(err)
	}
	data, err := store.Get("lintdec-oidc", b.key)
	if err != nil {
		t.Fatal(err)
	}
	var session oidcBrowserSession
	if err := json.Unmarshal([]byte(data), &session); err != nil {
		t.Fatal(err)
	}
	session.Subject = "another-account"
	if err := b.save(session); err != nil {
		t.Fatal(err)
	}
	b.expiry = time.Time{}
	if _, err := b.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
		t.Fatalf("account change: %v", err)
	}
	if m.refreshes != 0 {
		t.Fatal("refreshed another account")
	}
}

func TestOIDCBrowserRefreshDoesNotFollowRedirect(t *testing.T) {
	m := newBrowserIssuer(t)
	m.resource = "https://openrouter.ai/api/v1"
	b := browserForTest(t, m, newMemoryStore(), t.TempDir(), m.resource)
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatal(err)
	}
	m.redirectToken = true
	b.expiry = time.Time{}
	_, err := b.Token(context.Background())
	if err == nil || strings.Contains(err.Error(), "refresh-secret") {
		t.Fatalf("unsafe refresh error: %v", err)
	}
	if m.leaks.Load() != 0 {
		t.Fatal("refresh credential followed redirect")
	}
}

func TestOIDCBrowserCallbackDoesNotExposeSecrets(t *testing.T) {
	m := newBrowserIssuer(t)
	b := browserForTest(t, m, newMemoryStore(), t.TempDir())
	err := b.Login(context.Background(), func(authURL string) error {
		res, err := m.server.Client().Get(authURL)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			return err
		}
		if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Error("unsafe callback headers")
		}
		for _, secret := range []string{"code-secret", "access-secret", "refresh-secret"} {
			if strings.Contains(string(body), secret) {
				t.Error("callback exposed secret")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOIDCBrowserValidAccessTokenHash(t *testing.T) {
	m := newBrowserIssuer(t)
	m.atHash = "valid"
	b := browserForTest(t, m, newMemoryStore(), t.TempDir())
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCBrowserEndpointPolicy(t *testing.T) {
	m := newBrowserIssuer(t)
	b := browserForTest(t, m, newMemoryStore(), t.TempDir())
	for _, key := range []string{"response_type", "response_mode", "client_id", "redirect_uri", "scope", "state", "nonce", "code_challenge", "code_challenge_method", "resource", "audience", "request", "request_uri", "registration", "display", "claims_locales"} {
		m.authEndpoint = m.server.URL + "/authorize?" + key + "=attacker"
		if _, _, err := b.discover(context.Background()); err == nil {
			t.Errorf("accepted reserved authorization parameter %s", key)
		}
	}
	m.authEndpoint = m.server.URL + "/authorize?custom=value"
	if _, _, err := b.discover(context.Background()); err != nil {
		t.Fatalf("rejected safe endpoint query: %v", err)
	}
	for _, tt := range []struct {
		issuer, endpoint string
		want             bool
	}{
		{"https://example.com", "https://EXAMPLE.com:443/path", true},
		{"https://example.com:8443", "https://example.com:8443/path", true},
		{"https://example.com", "https://example.com:8443/path", false},
		{"https://example.com", "http://example.com/path", false},
		{"https://example.com", "https://other.example/path", false},
	} {
		if got := oidcSameOrigin(tt.issuer, tt.endpoint); got != tt.want {
			t.Errorf("same origin %q %q = %v", tt.issuer, tt.endpoint, got)
		}
	}
}

func TestOIDCBrowserPendingRefreshPreventsReplay(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*browserIssuer, *browserMemoryStore)
		rotated   bool
	}{
		{"dropped response", func(m *browserIssuer, _ *browserMemoryStore) { m.dropRefresh = true }, true},
		{"post-rotation write failure", func(_ *browserIssuer, store *browserMemoryStore) { store.failSetAt = 3 }, true},
		{"invalid refresh ID token", func(m *browserIssuer, _ *browserMemoryStore) { m.refreshID = "valid"; m.atHash = "wrong" }, true},
		{"invalid grant", func(m *browserIssuer, _ *browserMemoryStore) { m.invalidGrant = true }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newBrowserIssuer(t)
			store := newMemoryStore()
			dir := t.TempDir()
			cached := browserForTest(t, m, store, dir)
			if err := cached.Login(context.Background(), browserOpen(m)); err != nil {
				t.Fatal(err)
			}
			tt.configure(m, store)
			worker := browserForTest(t, m, store, dir)
			if _, err := worker.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
				t.Fatalf("refresh error: %v", err)
			}
			before := m.refreshes
			if (before == 1) != tt.rotated {
				t.Fatalf("rotation count %d", before)
			}
			data, err := store.Get("lintdec-oidc", cached.key)
			if err != nil {
				t.Fatal(err)
			}
			var session oidcBrowserSession
			if err := json.Unmarshal([]byte(data), &session); err != nil || !session.RefreshPending || session.RefreshToken != "" {
				t.Fatal("pending marker not persisted without refresh token")
			}
			fresh := browserForTest(t, m, store, dir)
			if _, err := fresh.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
				t.Fatalf("new instance: %v", err)
			}
			if _, err := cached.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
				t.Fatalf("cached instance: %v", err)
			}
			if m.refreshes != before {
				t.Fatal("replayed uncertain refresh token")
			}
			m.dropRefresh, m.invalidGrant, m.atHash, m.refreshID = false, false, "", ""
			if err := cached.Login(context.Background(), browserOpen(m)); err != nil {
				t.Fatalf("reauthentication: %v", err)
			}
			if _, err := fresh.Token(context.Background()); err != nil {
				t.Fatalf("refresh after login: %v", err)
			}
		})
	}
}

func TestOIDCBrowserPendingMarkerWriteFailsBeforeRefresh(t *testing.T) {
	m := newBrowserIssuer(t)
	store := newMemoryStore()
	dir := t.TempDir()
	b := browserForTest(t, m, store, dir)
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatal(err)
	}
	store.failSetAt = 2
	other := browserForTest(t, m, store, dir)
	if _, err := other.Token(context.Background()); err == nil {
		t.Fatal("marker write failure accepted")
	}
	if m.refreshes != 0 {
		t.Fatal("refresh sent without durable pending marker")
	}
	if access, err := other.Token(context.Background()); err != nil || access != "access-secret-1" {
		t.Fatalf("safe retry: %q %v", access, err)
	}
}

func TestOIDCBrowserCanceledRefreshRemainsPending(t *testing.T) {
	m := newBrowserIssuer(t)
	store := newMemoryStore()
	dir := t.TempDir()
	b := browserForTest(t, m, store, dir)
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatal(err)
	}
	m.refreshStarted = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := browserForTest(t, m, store, dir)
	done := make(chan error, 1)
	go func() { _, err := worker.Token(ctx); done <- err }()
	<-m.refreshStarted
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOIDCBrowserLoginRequired) {
		t.Fatalf("canceled refresh: %v", err)
	}
	fresh := browserForTest(t, m, store, dir)
	if _, err := fresh.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
		t.Fatalf("pending after cancellation: %v", err)
	}
}

func TestOIDCBrowserAllowsIATSkew(t *testing.T) {
	m := newBrowserIssuer(t)
	m.lifetime = "within skew"
	b := browserForTest(t, m, newMemoryStore(), t.TempDir())
	if err := b.Login(context.Background(), browserOpen(m)); err != nil {
		t.Fatalf("one minute skew: %v", err)
	}
}

func TestOIDCBrowserRefreshTokenResponsePresence(t *testing.T) {
	for _, tt := range []struct {
		name, contentType, response string
		valid                       bool
	}{
		{"JSON omitted", "", "omitted", true},
		{"JSON rotated", "", "", true},
		{"JSON empty", "", "empty", false},
		{"JSON null", "", "null", false},
		{"JSON case-insensitive null", "", "upper-null", false},
		{"JSON duplicate", "", "duplicate", false},
		{"form omitted", "application/x-www-form-urlencoded", "omitted", true},
		{"form rotated", "application/x-www-form-urlencoded", "", true},
		{"form empty", "application/x-www-form-urlencoded", "empty", false},
		{"form null", "application/x-www-form-urlencoded", "null", false},
		{"form differently-cased token", "application/x-www-form-urlencoded", "upper-rotated", false},
		{"form duplicate", "application/x-www-form-urlencoded", "duplicate", false},
		{"text empty", "text/plain", "empty", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newBrowserIssuer(t)
			store := newMemoryStore()
			dir := t.TempDir()
			b := browserForTest(t, m, store, dir)
			if err := b.Login(context.Background(), browserOpen(m)); err != nil {
				t.Fatal(err)
			}
			m.refreshResponse, m.refreshContentType = tt.response, tt.contentType
			worker := browserForTest(t, m, store, dir)
			_, err := worker.Token(context.Background())
			if tt.valid && err != nil {
				t.Fatalf("valid refresh response rejected: %v", err)
			}
			if !tt.valid && !errors.Is(err, ErrOIDCBrowserLoginRequired) {
				t.Fatalf("invalid refresh response accepted: %v", err)
			}
			data, err := store.Get("lintdec-oidc", b.key)
			if err != nil {
				t.Fatal(err)
			}
			var session oidcBrowserSession
			if err := json.Unmarshal([]byte(data), &session); err != nil {
				t.Fatal(err)
			}
			if !tt.valid {
				if !session.RefreshPending || session.RefreshToken != "" {
					t.Fatal("invalid response cleared pending marker")
				}
				if _, err := browserForTest(t, m, store, dir).Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
					t.Fatalf("replay guard: %v", err)
				}
				if m.refreshes != 1 {
					t.Fatalf("invalid token replayed: %d refreshes", m.refreshes)
				}
			} else {
				if session.RefreshPending || session.RefreshToken == "" {
					t.Fatal("valid response did not persist refresh token")
				}
				if tt.response == "omitted" && session.RefreshToken != "refresh-secret-0" {
					t.Fatal("omitted token did not preserve original")
				}
				if _, err := browserForTest(t, m, store, dir).Token(context.Background()); err != nil {
					t.Fatalf("subsequent refresh: %v", err)
				}
			}
		})
	}
}

func TestOIDCBrowserCancellationDuringPendingWrite(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		failSetAt, failAfterSetAt int
		loginRequired, pending    bool
	}{
		{"restored", 0, 0, false, false},
		{"pending write fails", 2, 0, false, false},
		{"pending write commits but returns error", 0, 2, false, false},
		{"restore fails", 3, 0, true, true},
		{"restore commits but returns error", 0, 3, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newBrowserIssuer(t)
			store := newMemoryStore()
			dir := t.TempDir()
			b := browserForTest(t, m, store, dir)
			if err := b.Login(context.Background(), browserOpen(m)); err != nil {
				t.Fatal(err)
			}
			original, err := store.Get("lintdec-oidc", b.key)
			if err != nil {
				t.Fatal(err)
			}
			store.blockSetAt, store.setStarted, store.unblockSet = 2, make(chan struct{}), make(chan struct{})
			store.failSetAt, store.failAfterSetAt = tt.failSetAt, tt.failAfterSetAt
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := browserForTest(t, m, store, dir).Token(ctx); done <- err }()
			<-store.setStarted
			cancel()
			close(store.unblockSet)
			err = <-done
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrOIDCBrowserLoginRequired) != tt.loginRequired {
				t.Fatalf("cancellation result: %v", err)
			}
			if m.refreshes != 0 {
				t.Fatal("refresh sent after cancellation during pending write")
			}
			stored, err := store.Get("lintdec-oidc", b.key)
			if err != nil {
				t.Fatal(err)
			}
			fresh := browserForTest(t, m, store, dir)
			if tt.pending {
				var session oidcBrowserSession
				if err := json.Unmarshal([]byte(stored), &session); err != nil || !session.RefreshPending || session.RefreshToken != "" {
					t.Fatal("failed restore did not leave pending record")
				}
				if _, err := fresh.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
					t.Fatalf("restore failure: %v", err)
				}
			} else {
				if stored != original {
					t.Fatal("original session was not restored")
				}
				if _, err := fresh.Token(context.Background()); err != nil {
					t.Fatalf("restored session unusable: %v", err)
				}
			}
		})
	}
}

func TestOIDCBrowserRefreshOptionalFieldsInForm(t *testing.T) {
	for _, tt := range []struct {
		name, contentType, id, scope string
		valid                        bool
	}{
		{"form omitted", "application/x-www-form-urlencoded", "", "", true},
		{"form present", "application/x-www-form-urlencoded", "valid", "openid offline_access", true},
		{"form missing scope", "application/x-www-form-urlencoded", "", "openid", false},
		{"form empty ID token", "application/x-www-form-urlencoded", "empty-id", "", false},
		{"JSON null ID token", "", "null-id", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newBrowserIssuer(t)
			store := newMemoryStore()
			b := browserForTest(t, m, store, t.TempDir())
			if err := b.Login(context.Background(), browserOpen(m)); err != nil {
				t.Fatal(err)
			}
			m.refreshContentType, m.refreshID, m.refreshScope = tt.contentType, tt.id, tt.scope
			if tt.id == "valid" {
				m.atHash = "valid"
			}
			b.expiry = time.Time{}
			_, err := b.Token(context.Background())
			if (err == nil) != tt.valid {
				t.Fatalf("optional fields result: %v", err)
			}
			if !tt.valid && !errors.Is(err, ErrOIDCBrowserLoginRequired) {
				t.Fatalf("invalid response did not fail closed: %v", err)
			}
		})
	}
}

func TestOIDCBrowserPendingWriteCommitErrorRestoresSession(t *testing.T) {
	for _, failRestore := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore-fails-%v", failRestore), func(t *testing.T) {
			m := newBrowserIssuer(t)
			store := newMemoryStore()
			dir := t.TempDir()
			b := browserForTest(t, m, store, dir)
			if err := b.Login(context.Background(), browserOpen(m)); err != nil {
				t.Fatal(err)
			}
			original, err := store.Get("lintdec-oidc", b.key)
			if err != nil {
				t.Fatal(err)
			}
			store.failAfterSetAt = 2
			if failRestore {
				store.failSetAt = 3
			}
			worker := browserForTest(t, m, store, dir)
			_, err = worker.Token(context.Background())
			if err == nil || errors.Is(err, ErrOIDCBrowserLoginRequired) != failRestore || !strings.Contains(err.Error(), "OIDC keychain write failed") || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("pending write result: %v", err)
			}
			if m.refreshes != 0 {
				t.Fatal("refresh sent after pending write error")
			}
			stored, err := store.Get("lintdec-oidc", b.key)
			if err != nil {
				t.Fatal(err)
			}
			fresh := browserForTest(t, m, store, dir)
			if failRestore {
				var session oidcBrowserSession
				if json.Unmarshal([]byte(stored), &session) != nil || !session.RefreshPending || session.RefreshToken != "" {
					t.Fatal("failed restore did not leave pending record")
				}
				if _, err := fresh.Token(context.Background()); !errors.Is(err, ErrOIDCBrowserLoginRequired) {
					t.Fatalf("failed restore: %v", err)
				}
				if m.refreshes != 0 {
					t.Fatal("refresh sent after failed restore")
				}
			} else {
				if stored != original {
					t.Fatal("original session was not restored")
				}
				if _, err := fresh.Token(context.Background()); err != nil {
					t.Fatalf("restored session unusable: %v", err)
				}
			}
		})
	}
}
