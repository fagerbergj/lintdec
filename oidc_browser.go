package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gofrs/flock"
	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
)

// OIDCBrowserOptions identifies a registered public client and its requested extra scopes.
// Include offline_access if the issuer requires it to issue refresh tokens.
type OIDCBrowserOptions struct {
	Issuer   string
	ClientID string
	Resource string
	Scopes   []string
}

var ErrOIDCBrowserLoginRequired = errors.New("OIDC browser login required; run 'lintdec provider login <name>'")

type oidcBrowserStore interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
}

type oidcLimitedTransport struct{ base http.RoundTripper }

func (t oidcLimitedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil {
		resp.Body = http.MaxBytesReader(nil, resp.Body, 1<<20)
	}
	return resp, err
}

type oidcResourceTransport struct {
	base     http.RoundTripper
	endpoint string
	resource string
}

func (t oidcResourceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.URL.String() != t.endpoint {
		return t.base.RoundTrip(r)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	_ = r.Body.Close()
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("invalid OIDC token request")
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, errors.New("invalid OIDC token request")
	}
	if form.Get("grant_type") == "refresh_token" {
		form.Set("resource", t.resource)
		body = []byte(form.Encode())
	}
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(strings.NewReader(string(body)))
	clone.ContentLength = int64(len(body))
	clone.GetBody = nil
	return t.base.RoundTrip(clone)
}

type oidcRefreshResponseTransport struct {
	base                         http.RoundTripper
	scopePresent, idTokenPresent bool
}

var errRefreshResponse = errors.New("invalid OIDC refresh response")

func (t *oidcRefreshResponseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	_ = resp.Body.Close()
	if err != nil || len(body) > 1<<20 {
		return nil, errRefreshResponse
	}
	content, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if content == "application/x-www-form-urlencoded" || content == "text/plain" {
		err = t.checkForm(body)
	} else {
		err = t.checkJSON(body)
	}
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// checkForm rejects a form reply with an ambiguous or empty refresh token.
func (t *oidcRefreshResponseTransport) checkForm(body []byte) error {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return errRefreshResponse
	}
	_, t.scopePresent = values["scope"]
	_, t.idTokenPresent = values["id_token"]
	for key := range values {
		if key != "refresh_token" && strings.EqualFold(key, "refresh_token") {
			return errRefreshResponse
		}
	}
	if tokens, present := values["refresh_token"]; present && (len(tokens) != 1 || tokens[0] == "" || tokens[0] == "null") {
		return errRefreshResponse
	}
	return nil
}

// checkJSON rejects a JSON reply that is not one object or repeats or empties its refresh token.
func (t *oidcRefreshResponseTransport) checkJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if start, err := decoder.Token(); err != nil || start != json.Delim('{') {
		return errRefreshResponse
	}
	seenRefresh := false
	for decoder.More() {
		name, value, err := jsonMember(decoder)
		if err != nil {
			return err
		}
		t.scopePresent = t.scopePresent || name == "scope"
		t.idTokenPresent = t.idTokenPresent || name == "id_token"
		if strings.EqualFold(name, "refresh_token") {
			if seenRefresh || !nonEmptyJSONString(value) {
				return errRefreshResponse
			}
			seenRefresh = true
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return errRefreshResponse
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errRefreshResponse
	}
	return nil
}

func jsonMember(decoder *json.Decoder) (string, json.RawMessage, error) {
	key, err := decoder.Token()
	if err != nil {
		return "", nil, errRefreshResponse
	}
	var value json.RawMessage
	if decoder.Decode(&value) != nil {
		return "", nil, errRefreshResponse
	}
	name, ok := key.(string)
	if !ok {
		return "", nil, errRefreshResponse
	}
	return name, value, nil
}

func nonEmptyJSONString(value json.RawMessage) bool {
	var s string
	return json.Unmarshal(value, &s) == nil && s != ""
}

type oidcSystemKeyring struct{}

func (oidcSystemKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (oidcSystemKeyring) Set(service, user, secret string) error {
	return keyring.Set(service, user, secret)
}

type oidcBrowserSession struct {
	RefreshToken   string   `json:"refresh_token"`
	RefreshPending bool     `json:"refresh_pending,omitempty"`
	Subject        string   `json:"subject"`
	Nonce          string   `json:"nonce"`
	Scopes         []string `json:"scopes"`
}

// OIDCBrowser is a standalone authSource; keychain access may wait for an OS unlock prompt. Processes sharing
// a session must run as the same OS account, and separate resource grants must not share a rotating refresh token.
type OIDCBrowser struct {
	issuer, clientID, resource, key, lockDir string
	scopes                                   []string
	store                                    oidcBrowserStore
	httpClient                               *http.Client
	gate                                     chan struct{}
	access, subject                          string
	expiry                                   time.Time
	refreshMargin                            time.Duration
}

// NewOIDCBrowser requires an HTTPS issuer and an explicitly registered public client ID.
// Browser login is unavailable on Windows until private locking is supported.
func NewOIDCBrowser(options OIDCBrowserOptions) (*OIDCBrowser, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("OIDC browser login is unavailable on Windows")
	}
	if !oidcHTTPSURL(options.Issuer, false) || options.ClientID == "" || strings.TrimSpace(options.ClientID) != options.ClientID {
		return nil, errors.New("OIDC requires an HTTPS issuer and a public client ID")
	}
	if options.Resource != "" && !validOIDCResource(options.Resource) {
		return nil, errors.New("invalid OIDC resource")
	}
	scopes, err := oidcScopes(options.Scopes)
	if err != nil {
		return nil, err
	}
	home, err := accountHome()
	if err != nil {
		return nil, err
	}
	identity, _ := json.Marshal([]string{options.Issuer, options.ClientID, options.Resource})
	hash := sha256.Sum256(identity)
	return &OIDCBrowser{
		issuer: options.Issuer, clientID: options.ClientID, resource: options.Resource, scopes: scopes,
		key: hex.EncodeToString(hash[:]), lockDir: filepath.Join(home, ".lintdec-oidc-locks"),
		store: oidcSystemKeyring{}, gate: make(chan struct{}, 1),
		httpClient: &http.Client{Timeout: 30 * time.Second, Transport: oidcLimitedTransport{http.DefaultTransport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func validOIDCResource(raw string) bool {
	resource, err := url.Parse(raw)
	return err == nil && resource.IsAbs() && !strings.Contains(raw, "#") && resource.User == nil
}

// oidcScopes: the requested scopes plus openid, sorted and unique, so the session key is stable.
func oidcScopes(requested []string) ([]string, error) {
	scopes := []string{"openid"}
	for _, scope := range requested {
		if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
			return nil, errors.New("invalid OIDC scope")
		}
		if scope != "openid" {
			scopes = append(scopes, scope)
		}
	}
	sort.Strings(scopes)
	return uniqueOIDCScopes(scopes), nil
}

// accountHome: the OS account's home from the user database, not $HOME, so locks cannot be redirected.
func accountHome() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", errors.New("OIDC account home unavailable")
	}
	account, err = user.LookupId(account.Uid)
	if err != nil || account.HomeDir == "" || !filepath.IsAbs(account.HomeDir) {
		return "", errors.New("OIDC account home unavailable")
	}
	return account.HomeDir, nil
}

func uniqueOIDCScopes(scopes []string) []string {
	out := scopes[:0]
	for _, s := range scopes {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func oidcHTTPSURL(raw string, query bool) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && !strings.Contains(raw, "#") && (query || (u.RawQuery == "" && !u.ForceQuery))
}

func oidcSameOrigin(issuer, endpoint string) bool {
	i, err := url.Parse(issuer)
	if err != nil {
		return false
	}
	e, err := url.Parse(endpoint)
	if err != nil || !oidcHTTPSURL(endpoint, true) {
		return false
	}
	port := func(u *url.URL) string {
		if u.Port() == "" {
			return "443"
		}
		return u.Port()
	}
	return strings.EqualFold(i.Hostname(), e.Hostname()) && port(i) == port(e)
}

func oidcSafeEndpointQuery(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for key, values := range query {
		if len(values) != 1 {
			return false
		}
		switch key {
		case "response_type", "response_mode", "client_id", "client_secret", "redirect_uri", "scope", "state", "nonce", "code_challenge", "code_challenge_method", "code_verifier", "code", "grant_type", "refresh_token", "access_token", "resource", "audience", "request", "request_uri", "registration", "prompt", "display", "max_age", "ui_locales", "claims_locales", "id_token_hint", "login_hint", "acr_values", "claims", "authorization_details":
			return false
		}
	}
	return true
}

func oidcBrowserError(ctx context.Context, message string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New(message)
}

func (b *OIDCBrowser) acquire(ctx context.Context) (func(), error) {
	select {
	case b.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	unlockGate := func() { <-b.gate }
	if err := os.MkdirAll(b.lockDir, 0700); err != nil {
		unlockGate()
		return nil, errors.New("OIDC lock directory unavailable")
	}
	info, err := os.Lstat(b.lockDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		unlockGate()
		return nil, errors.New("OIDC lock directory is not private")
	}
	lock := flock.New(filepath.Join(b.lockDir, b.key+".lock"))
	ok, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil || !ok {
		unlockGate()
		return nil, oidcBrowserError(ctx, "OIDC lock unavailable")
	}
	return func() { _ = lock.Unlock(); unlockGate() }, nil
}

func (b *OIDCBrowser) discover(ctx context.Context) (*oidc.Provider, oauth2.Config, error) {
	ctx = oidc.ClientContext(ctx, b.httpClient)
	p, err := oidc.NewProvider(ctx, b.issuer)
	if err != nil {
		return nil, oauth2.Config{}, oidcBrowserError(ctx, "OIDC discovery failed")
	}
	var metadata struct {
		JWKS        string   `json:"jwks_uri"`
		PKCEMethods []string `json:"code_challenge_methods_supported"`
	}
	ep := p.Endpoint()
	if p.Claims(&metadata) != nil || !slices.Contains(metadata.PKCEMethods, "S256") ||
		!oidcSameOrigin(b.issuer, ep.AuthURL) || !oidcSameOrigin(b.issuer, ep.TokenURL) || !oidcSameOrigin(b.issuer, metadata.JWKS) ||
		!oidcSafeEndpointQuery(ep.AuthURL) || !oidcSafeEndpointQuery(ep.TokenURL) {
		return nil, oauth2.Config{}, errors.New("OIDC discovery returned unsafe endpoints or unsupported PKCE")
	}
	ep.AuthStyle = oauth2.AuthStyleInParams
	return p, oauth2.Config{ClientID: b.clientID, Endpoint: ep, Scopes: b.scopes}, nil
}

func randomOIDCString() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", errors.New("OIDC random generator failed")
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func (b *OIDCBrowser) verify(ctx context.Context, p *oidc.Provider, raw, access, nonce, subject string, login bool) (string, error) {
	id, err := p.Verifier(&oidc.Config{ClientID: b.clientID}).Verify(ctx, raw)
	if err != nil || !b.idTokenMatches(id, subject, nonce, login) {
		return "", oidcBrowserError(ctx, "OIDC ID token verification failed")
	}
	var claims idTokenClaims
	if id.Claims(&claims) != nil || !b.authorizedParty(claims.AuthorizedParty, id.Audience) {
		return "", errors.New("OIDC ID token authorized party mismatch")
	}
	if !validLifetime(id, claims, time.Now()) {
		return "", errors.New("invalid OIDC ID token lifetime")
	}
	if !login && claims.Nonce != nil && !sameSecret(id.Nonce, nonce) {
		return "", errors.New("OIDC ID token nonce mismatch")
	}
	if claims.AccessHash != nil && (id.AccessTokenHash == "" || id.VerifyAccessToken(access) != nil) {
		return "", errors.New("OIDC access token hash mismatch")
	}
	return id.Subject, nil
}

// idTokenClaims are raw so a present-but-null claim is told apart from an omitted one.
type idTokenClaims struct {
	AuthorizedParty string          `json:"azp"`
	AccessHash      json.RawMessage `json:"at_hash"`
	Nonce           json.RawMessage `json:"nonce"`
	IssuedAt        json.RawMessage `json:"iat"`
	Expiry          json.RawMessage `json:"exp"`
}

func sameSecret(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// idTokenMatches: our issuer, the expected subject, and the login nonce (on refresh, any nonce must match).
func (b *OIDCBrowser) idTokenMatches(id *oidc.IDToken, subject, nonce string, login bool) bool {
	if id.Issuer != b.issuer || id.Subject == "" || (subject != "" && id.Subject != subject) {
		return false
	}
	if login {
		return sameSecret(id.Nonce, nonce)
	}
	return id.Nonce == "" || sameSecret(id.Nonce, nonce)
}

// authorizedParty: azp, when present or when there are several audiences, must be this client.
func (b *OIDCBrowser) authorizedParty(azp string, audience []string) bool {
	return (azp == "" || azp == b.clientID) && (len(audience) <= 1 || azp == b.clientID)
}

func validLifetime(id *oidc.IDToken, claims idTokenClaims, now time.Time) bool {
	return numericClaim(claims.IssuedAt) && numericClaim(claims.Expiry) && !id.IssuedAt.IsZero() &&
		!id.IssuedAt.After(now.Add(time.Minute)) && id.Expiry.After(id.IssuedAt) && id.Expiry.After(now)
}

func numericClaim(raw json.RawMessage) bool {
	var n float64
	return len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &n) == nil
}

func (b *OIDCBrowser) validateToken(t *oauth2.Token, scopePresent bool) error {
	if t == nil || t.AccessToken == "" || !strings.EqualFold(t.TokenType, "Bearer") || t.Expiry.IsZero() || !t.Expiry.After(time.Now()) {
		return errors.New("invalid OIDC token response")
	}
	if scopePresent {
		s, ok := t.Extra("scope").(string)
		if !ok {
			return errors.New("invalid OIDC token scope")
		}
		granted := strings.Fields(s)
		for _, requested := range b.scopes {
			found := false
			for _, g := range granted {
				if g == requested {
					found = true
					break
				}
			}
			if !found {
				return errors.New("OIDC token missing requested scope")
			}
		}
	}
	return nil
}

func (b *OIDCBrowser) cacheToken(token *oauth2.Token) {
	b.access, b.expiry = token.AccessToken, token.Expiry
	b.refreshMargin = min(10*time.Second, time.Until(token.Expiry)/5)
}

func (b *OIDCBrowser) save(session oidcBrowserSession) error {
	data, err := json.Marshal(session)
	if err != nil || b.store.Set("lintdec-oidc", b.key, string(data)) != nil {
		return errors.New("OIDC keychain write failed")
	}
	return nil
}

// Login starts a loopback callback and calls openBrowser with the authorization URL.
// openBrowser must return promptly; the caller supplies the platform browser launcher.
func (b *OIDCBrowser) Login(ctx context.Context, openBrowser func(string) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if openBrowser == nil {
		return errors.New("OIDC browser opener required")
	}
	unlock, err := b.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	p, cfg, err := b.discover(ctx)
	if err != nil {
		return err
	}
	secrets, err := randomOIDCStrings(3)
	if err != nil {
		return err
	}
	state, nonce, verifier := secrets[0], secrets[1], secrets[2]
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return errors.New("OIDC loopback listener failed")
	}
	defer listener.Close()
	cfg.RedirectURL = "http://" + listener.Addr().String() + "/callback"
	results := make(chan loginCallback, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10, Handler: b.callbackHandler(listener.Addr().String(), state, results)}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	authOptions := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)}
	exchangeOptions := []oauth2.AuthCodeOption{oauth2.VerifierOption(verifier)}
	if b.resource != "" {
		authOptions = append(authOptions, oauth2.SetAuthURLParam("resource", b.resource))
		exchangeOptions = append(exchangeOptions, oauth2.SetAuthURLParam("resource", b.resource))
	}
	if err := openBrowser(cfg.AuthCodeURL(state, authOptions...)); err != nil {
		return errors.New("OIDC browser launch failed")
	}
	var callback loginCallback
	select {
	case callback = <-results:
	case <-ctx.Done():
		return ctx.Err()
	}
	if callback.failed {
		return errors.New("OIDC authorization failed")
	}
	token, err := cfg.Exchange(oidc.ClientContext(ctx, b.httpClient), callback.code, exchangeOptions...)
	if err != nil {
		return oidcBrowserError(ctx, "OIDC code exchange failed")
	}
	return b.finishLogin(ctx, p, token, nonce)
}

func randomOIDCStrings(n int) ([]string, error) {
	out := make([]string, n)
	for i := range out {
		var err error
		if out[i], err = randomOIDCString(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type loginCallback struct {
	code   string
	failed bool
}

// callbackHandler accepts one redirect that carries our state, from our loopback host, for our issuer.
func (b *OIDCBrowser) callbackHandler(host, state string, results chan<- loginCallback) http.Handler {
	send := func(c loginCallback) {
		select {
		case results <- c:
		default:
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		q := r.URL.Query()
		if !b.ownCallback(r, q, host, state) {
			http.Error(w, "Invalid callback", http.StatusBadRequest)
			return
		}
		if len(q["code"]) != 1 || q.Get("code") == "" || len(q["error"]) != 0 {
			send(loginCallback{failed: true})
			http.Error(w, "Authorization failed", http.StatusBadRequest)
			return
		}
		send(loginCallback{code: q.Get("code")})
		_, _ = io.WriteString(w, "Authorization received. You may close this window.")
	})
}

func (b *OIDCBrowser) ownCallback(r *http.Request, q url.Values, host, state string) bool {
	return r.Method == http.MethodGet && r.Host == host && r.URL.Path == "/callback" && len(q["state"]) == 1 &&
		sameSecret(q.Get("state"), state) && len(q["iss"]) <= 1 && (q.Get("iss") == "" || q.Get("iss") == b.issuer)
}

// finishLogin keeps the session only once the tokens and the ID token all verify.
func (b *OIDCBrowser) finishLogin(ctx context.Context, p *oidc.Provider, token *oauth2.Token, nonce string) error {
	if err := b.validateToken(token, token.Extra("scope") != nil); err != nil {
		return err
	}
	if token.RefreshToken == "" {
		return errors.New("OIDC refresh token missing")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return errors.New("OIDC ID token missing")
	}
	subject, err := b.verify(ctx, p, raw, token.AccessToken, nonce, "", true)
	if err != nil {
		return err
	}
	if err := b.save(oidcBrowserSession{RefreshToken: token.RefreshToken, Subject: subject, Nonce: nonce, Scopes: b.scopes}); err != nil {
		return err
	}
	b.subject = subject
	b.cacheToken(token)
	return nil
}

func oidcRefreshFailure(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.Join(ErrOIDCBrowserLoginRequired, ctx.Err())
	}
	return ErrOIDCBrowserLoginRequired
}

// Token returns the current access token, refreshing it from the OS keychain when needed.
func (b *OIDCBrowser) Token(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	unlock, err := b.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	session, err := b.loadSession()
	if err != nil {
		return "", err
	}
	if b.access != "" && time.Until(b.expiry) > b.refreshMargin {
		return b.access, nil
	}
	p, cfg, err := b.discover(ctx)
	if err != nil {
		return "", err
	}
	client := *b.httpClient
	if b.resource != "" {
		endpoint, _ := url.Parse(cfg.Endpoint.TokenURL)
		client.Transport = oidcResourceTransport{base: client.Transport, endpoint: endpoint.String(), resource: b.resource}
	}
	responseTransport := &oidcRefreshResponseTransport{base: client.Transport}
	client.Transport = responseTransport
	if err := ctx.Err(); err != nil {
		return "", err
	}
	refreshToken, err := b.markRefreshPending(ctx, &session)
	if err != nil {
		return "", err
	}
	// A pending record must survive any uncertain refresh outcome to prevent replay.
	token, err := cfg.TokenSource(oidc.ClientContext(ctx, &client), &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil || b.checkRefreshed(ctx, p, token, responseTransport, session) != nil {
		return "", oidcRefreshFailure(ctx)
	}
	session.RefreshToken = token.RefreshToken
	session.RefreshPending = false
	if err := b.save(session); err != nil {
		return "", errors.Join(ErrOIDCBrowserLoginRequired, err)
	}
	b.subject = session.Subject
	b.cacheToken(token)
	return b.access, nil
}

// loadSession reads the keychain session; a pending, rescoped, or foreign one needs a new login.
func (b *OIDCBrowser) loadSession() (oidcBrowserSession, error) {
	var session oidcBrowserSession
	data, err := b.store.Get("lintdec-oidc", b.key)
	if errors.Is(err, keyring.ErrNotFound) {
		return session, ErrOIDCBrowserLoginRequired
	}
	if err != nil {
		return session, errors.New("OIDC keychain read failed")
	}
	if json.Unmarshal([]byte(data), &session) != nil || session.Subject == "" || session.Nonce == "" {
		return session, errors.New("invalid OIDC keychain session")
	}
	if session.RefreshPending || session.RefreshToken == "" || !slices.Equal(session.Scopes, b.scopes) || (b.subject != "" && b.subject != session.Subject) {
		return session, ErrOIDCBrowserLoginRequired
	}
	return session, nil
}

// markRefreshPending durably removes the refresh token before it is spent, restoring it if that fails.
func (b *OIDCBrowser) markRefreshPending(ctx context.Context, session *oidcBrowserSession) (string, error) {
	refreshToken := session.RefreshToken
	session.RefreshToken = ""
	session.RefreshPending = true
	pendingErr := b.save(*session)
	ctxErr := ctx.Err()
	if ctxErr == nil && pendingErr == nil {
		return refreshToken, nil
	}
	session.RefreshToken, session.RefreshPending = refreshToken, false
	if restoreErr := b.save(*session); restoreErr != nil {
		return "", errors.Join(ErrOIDCBrowserLoginRequired, ctxErr, pendingErr, restoreErr)
	}
	return "", errors.Join(ctxErr, pendingErr)
}

// checkRefreshed: a valid rotated token, and a verified ID token whenever the reply carried one.
func (b *OIDCBrowser) checkRefreshed(ctx context.Context, p *oidc.Provider, token *oauth2.Token, rt *oidcRefreshResponseTransport, session oidcBrowserSession) error {
	if err := b.validateToken(token, rt.scopePresent); err != nil {
		return err
	}
	if token.RefreshToken == "" {
		return errors.New("OIDC refresh token missing")
	}
	if !rt.idTokenPresent {
		return nil
	}
	id, ok := token.Extra("id_token").(string)
	if !ok || id == "" {
		return errors.New("OIDC ID token missing")
	}
	_, err := b.verify(ctx, p, id, token.AccessToken, session.Nonce, session.Subject, false)
	return err
}

func (b *OIDCBrowser) token(ctx context.Context) (string, error) { return b.Token(ctx) }

var _ authSource = (*OIDCBrowser)(nil)
