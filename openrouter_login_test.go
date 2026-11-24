package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenRouterBrowserOAuthUsesPKCEAndPrivateCallback(t *testing.T) {
	var challenge string
	var exchanges atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		exchanges.Add(1)
		if r.URL.String() != "https://openrouter.ai/api/v1/auth/keys" || r.Method != "POST" {
			t.Errorf("wrong exchange endpoint")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		hash := sha256.Sum256([]byte(body["code_verifier"]))
		if body["code"] != "authorization-code" || body["code_challenge_method"] != "S256" || base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
			t.Error("invalid PKCE exchange")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"key":"sk-or-v1-test"}`)), Request: r}, nil
	})}
	open := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		if u.Hostname() != "openrouter.ai" || u.Path != "/auth" || u.Query().Get("code_challenge_method") != "S256" {
			return fmt.Errorf("bad authorization URL")
		}
		challenge = u.Query().Get("code_challenge")
		callback, err := url.Parse(u.Query().Get("callback_url"))
		if err != nil {
			return err
		}
		if callback.Hostname() != "127.0.0.1" || !strings.HasPrefix(callback.Path, "/callback/") || len(strings.TrimPrefix(callback.Path, "/callback/")) < 32 {
			return fmt.Errorf("callback is not an unguessable loopback URL")
		}
		go func() {
			q := callback.Query()
			q.Set("code", "authorization-code")
			callback.RawQuery = q.Encode()
			wrong := *callback
			wrong.Path = "/callback/wrong"
			if resp, err := http.Get(wrong.String()); err == nil {
				resp.Body.Close()
				if resp.StatusCode != 404 {
					t.Errorf("accepted wrong callback path")
				}
			}
			if resp, err := http.Get(callback.String()); err == nil {
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Errorf("valid callback status %d", resp.StatusCode)
				}
			} else {
				t.Error(err)
			}
		}()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, err := loginOpenRouterOAuth(ctx, open, client)
	if err != nil || key != "sk-or-v1-test" || exchanges.Load() != 1 {
		t.Fatalf("key=%q err=%v exchanges=%d", key, err, exchanges.Load())
	}
}

func TestOpenRouterBrowserOAuthDeniedMakesNoExchange(t *testing.T) {
	var exchanges atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { exchanges.Add(1); return nil, fmt.Errorf("unexpected") })}
	open := func(raw string) error {
		u, _ := url.Parse(raw)
		callback, _ := url.Parse(u.Query().Get("callback_url"))
		q := callback.Query()
		q.Set("error", "access_denied")
		callback.RawQuery = q.Encode()
		go func() {
			if resp, err := http.Get(callback.String()); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := loginOpenRouterOAuth(ctx, open, client)
	if err == nil || !strings.Contains(err.Error(), "denied") || exchanges.Load() != 0 {
		t.Fatalf("err=%v exchanges=%d", err, exchanges.Load())
	}
}

func TestOpenRouterBrowserOAuthCancellationMakesNoExchange(t *testing.T) {
	var exchanges atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { exchanges.Add(1); return nil, fmt.Errorf("unexpected") })}
	ctx, cancel := context.WithCancel(context.Background())
	open := func(raw string) error { cancel(); return nil }
	_, err := loginOpenRouterOAuth(ctx, open, client)
	if err == nil || exchanges.Load() != 0 {
		t.Fatalf("err=%v exchanges=%d", err, exchanges.Load())
	}
}

func TestOpenRouterBrowserOAuthRedactsExchangeErrors(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("sensitive-code")), Request: r}, nil
	})}
	open := func(raw string) error {
		u, _ := url.Parse(raw)
		callback, _ := url.Parse(u.Query().Get("callback_url"))
		q := callback.Query()
		q.Set("code", "sensitive-code")
		callback.RawQuery = q.Encode()
		go func() {
			if resp, err := http.Get(callback.String()); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := loginOpenRouterOAuth(ctx, open, client)
	if err == nil || strings.Contains(err.Error(), "sensitive-code") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("unsafe error: %v", err)
	}
}
