package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

func loginOpenRouterOAuth(ctx context.Context, open func(string) error, client *http.Client) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	random := make([]byte, 64)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate login challenge: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(random[:32])
	callbackPath := "/callback/" + base64.RawURLEncoding.EncodeToString(random[32:])
	challenge := sha256.Sum256([]byte(verifier))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("start login callback: %w", err)
	}
	callback := "http://" + listener.Addr().String() + callbackPath
	type callbackResult struct {
		code string
		err  error
	}
	results := make(chan callbackResult, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != callbackPath || r.Host != listener.Addr().String() {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("error") != "" {
			select {
			case results <- callbackResult{err: fmt.Errorf("OpenRouter sign-in was denied")}:
			default:
			}
			http.Error(w, "Sign-in denied. Return to your terminal.", http.StatusBadRequest)
			return
		}
		if len(q["code"]) != 1 || q.Get("code") == "" {
			http.Error(w, "Invalid sign-in callback", http.StatusBadRequest)
			return
		}
		select {
		case results <- callbackResult{code: q.Get("code")}:
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprintln(w, "Authorization received. Return to your terminal.")
		default:
			http.Error(w, "Sign-in already handled", http.StatusConflict)
		}
	})
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	defer server.Close()
	authorize := url.URL{Scheme: "https", Host: "openrouter.ai", Path: "/auth"}
	authorize.RawQuery = url.Values{
		"callback_url":          {callback},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"key_label":             {"lintdec"},
	}.Encode()
	if err := open(authorize.String()); err != nil {
		return "", fmt.Errorf("open sign-in page: %w", err)
	}
	var result callbackResult
	select {
	case result = <-results:
		if result.err != nil {
			return "", result.err
		}
	case err := <-serverErr:
		return "", fmt.Errorf("login callback failed: %w", err)
	case <-ctx.Done():
		return "", ctx.Err()
	}
	body, err := json.Marshal(map[string]string{"code": result.code, "code_verifier": verifier, "code_challenge_method": "S256"})
	if err != nil {
		return "", fmt.Errorf("prepare sign-in exchange")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://openrouter.ai/api/v1/auth/keys", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("prepare sign-in exchange")
	}
	req.Header.Set("Content-Type", "application/json")
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := copyClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("OpenRouter sign-in exchange failed; a key may have been created; check https://openrouter.ai/settings/keys")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenRouter sign-in exchange: HTTP %d; a key may have been created; check https://openrouter.ai/settings/keys", res.StatusCode)
	}
	var reply struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&reply); err != nil || reply.Key == "" {
		return "", fmt.Errorf("invalid OpenRouter sign-in response; a key may have been created; check https://openrouter.ai/settings/keys")
	}
	return reply.Key, nil
}
