package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

type authSource interface {
	token(context.Context) (string, error)
}

type staticSource string

func (s staticSource) token(context.Context) (string, error) { return string(s), nil }

func trustedHTTPSBase(raw string) (*url.URL, bool) {
	return httpsBase(raw, false)
}

func httpsBase(raw string, allowPort bool) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || !plainURL(u) || (!allowPort && u.Host != u.Hostname()) {
		return nil, false
	}
	p := strings.TrimRight(u.Path, "/")
	if p != "" && path.Clean(p) != p {
		return nil, false
	}
	return u, true
}

// plainURL: no credentials, query, fragment, or escaped path that could point a credential elsewhere.
func plainURL(u *url.URL) bool {
	return u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == ""
}

func openRouterAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

func providerAuth(p Provider) (authSource, error) {
	switch {
	case p.Auth == "" || p.Auth == "api-key":
		return apiKeyAuth(p)
	case p.APIKey != "" || p.APIKeyRef != "":
		return nil, fmt.Errorf("dynamic authentication cannot be combined with an API key")
	case p.Auth == "oidc":
		if _, ok := httpsBase(p.BaseURL, true); !ok || p.OIDCBaseURL == "" || strings.TrimRight(p.BaseURL, "/") != p.OIDCBaseURL {
			return nil, fmt.Errorf("OIDC requires its configured HTTPS endpoint; add a new provider to change it")
		}
		return NewOIDCBrowser(OIDCBrowserOptions{Issuer: p.OIDCIssuerURL, ClientID: p.OIDCClientID, Resource: p.OIDCResource, Scopes: p.OIDCScopes})
	case p.Auth == "openrouter-oidc":
		return openRouterOIDCAuth(p)
	}
	if _, ok := trustedHTTPSBase(p.BaseURL); !ok {
		return nil, fmt.Errorf("dynamic authentication requires a trusted HTTPS endpoint")
	}
	return nil, fmt.Errorf("unsupported authentication method %q", p.Auth)
}

// apiKeyAuth: a keychain key is only released to the endpoint it was saved for.
func apiKeyAuth(p Provider) (authSource, error) {
	if p.APIKey != "" || p.APIKeyRef == "" {
		return nil, nil
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || !plainURL(u) || strings.TrimRight(p.BaseURL, "/") != p.APIKeyBaseURL {
		return nil, fmt.Errorf("keychain API key requires its configured endpoint; add a new provider to change it")
	}
	return &keychainAPIKeySource{ref: p.APIKeyRef}, nil
}

func openRouterOIDCAuth(p Provider) (authSource, error) {
	u, ok := trustedHTTPSBase(p.BaseURL)
	if !ok {
		return nil, fmt.Errorf("dynamic authentication requires a trusted HTTPS endpoint")
	}
	if u.Hostname() != "openrouter.ai" || !openRouterAPIPath(u.Path) {
		return nil, fmt.Errorf("OpenRouter OIDC requires the OpenRouter API endpoint")
	}
	file := firstNonEmpty(p.OIDCTokenFile, os.Getenv("OPENROUTER_OIDC_TOKEN_FILE"))
	policy := firstNonEmpty(p.FederationPolicyID, os.Getenv("OPENROUTER_FEDERATION_POLICY_ID"))
	if file == "" || policy == "" {
		return nil, fmt.Errorf("OpenRouter OIDC needs a token file and federation policy ID")
	}
	return newOIDCSource(file, policy, "https://openrouter.ai/api/v1/oauth/token", &http.Client{Timeout: 30 * time.Second}), nil
}

func reportsProviderCost(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" {
		return false
	}
	return u.Hostname() == "openrouter.ai" && openRouterAPIPath(u.Path)
}

func authorizeRequest(req *http.Request, source authSource, key string) error {
	if source != nil {
		var err error
		key, err = source.token(req.Context())
		if err != nil {
			return err
		}
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return nil
}

func sendRequest(client *http.Client, req *http.Request, noRedirect bool) (*http.Response, error) {
	if noRedirect {
		copyClient := *client
		copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copyClient
	}
	return client.Do(req)
}

func responseError(res *http.Response, source authSource, key string) string {
	if source != nil || key != "" {
		return fmt.Sprintf("HTTP %d", res.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 400))
	return fmt.Sprintf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
}

func authorizedDo(client *http.Client, req *http.Request, source authSource, key string) (*http.Response, error) {
	if err := authorizeRequest(req, source, key); err != nil {
		return nil, err
	}
	return sendRequest(client, req, source != nil || key != "")
}

type oidcSource struct {
	file, policy, endpoint string
	httpc                  *http.Client
	mu                     sync.Mutex
	access                 string
	expires                time.Time
	refreshMargin          time.Duration
	refreshing             chan struct{}
}

func newOIDCSource(file, policy, endpoint string, client *http.Client) *oidcSource {
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &oidcSource{file: file, policy: policy, endpoint: endpoint, httpc: &copyClient}
}

func (s *oidcSource) token(ctx context.Context) (string, error) {
	for {
		s.mu.Lock()
		if s.access != "" && time.Until(s.expires) > s.refreshMargin {
			token := s.access
			s.mu.Unlock()
			return token, nil
		}
		if wait := s.refreshing; wait != nil {
			s.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		s.refreshing = make(chan struct{})
		s.mu.Unlock()
		token, expiry, err := s.exchange(ctx)
		s.mu.Lock()
		if err == nil {
			s.access, s.expires = token, expiry
			s.refreshMargin = min(30*time.Second, time.Until(expiry)/5)
		}
		close(s.refreshing)
		s.refreshing = nil
		s.mu.Unlock()
		return token, err
	}
}

// readSubjectToken reads the rotating workload token fresh for each exchange.
func readSubjectToken(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read OIDC token file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil {
		return "", fmt.Errorf("read OIDC token file: %w", err)
	}
	jwt := strings.TrimSpace(string(data))
	if jwt == "" || len(jwt) > 16384 {
		return "", fmt.Errorf("OIDC token file is empty or too large")
	}
	return jwt, nil
}

type tokenExchangeReply struct {
	AccessToken     string `json:"access_token"`
	TokenType       string `json:"token_type"`
	IssuedTokenType string `json:"issued_token_type"`
	ExpiresIn       int    `json:"expires_in"`
	Scope           string `json:"scope"`
}

// inferenceToken: a bearer access token scoped to inference, or none.
func (r tokenExchangeReply) inferenceToken() bool {
	return r.AccessToken != "" && strings.EqualFold(r.TokenType, "Bearer") && r.IssuedTokenType == "urn:ietf:params:oauth:token-type:access_token" &&
		r.ExpiresIn > 0 && (r.Scope == "" || r.Scope == "inference")
}

func (s *oidcSource) exchange(ctx context.Context) (string, time.Time, error) {
	jwt, err := readSubjectToken(s.file)
	if err != nil {
		return "", time.Time{}, err
	}
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:jwt"},
		"federation_policy_id": {s.policy},
		"subject_token":        {jwt},
		"scope":                {"inference"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("invalid token exchange endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	started := time.Now()
	res, err := s.httpc.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("OpenRouter token exchange failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("OpenRouter token exchange failed: HTTP %d", res.StatusCode)
	}
	var reply tokenExchangeReply
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&reply); err != nil {
		return "", time.Time{}, fmt.Errorf("invalid OpenRouter token response")
	}
	if !reply.inferenceToken() {
		return "", time.Time{}, fmt.Errorf("invalid OpenRouter inference token response")
	}
	expires := started.Add(time.Duration(min(reply.ExpiresIn, 900)) * time.Second)
	if !expires.After(time.Now()) {
		return "", time.Time{}, fmt.Errorf("OpenRouter returned an expired inference token")
	}
	return reply.AccessToken, expires, nil
}
