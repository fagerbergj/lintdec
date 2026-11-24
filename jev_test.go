package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

type jevTestTokenSource func(context.Context) (string, error)

func (s jevTestTokenSource) token(ctx context.Context) (string, error) { return s(ctx) }

func TestFetchJevModels(t *testing.T) {
	calls := 0
	source := jevTestTokenSource(func(context.Context) (string, error) {
		calls++
		return fmt.Sprintf("test-token-%d", calls), nil
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/models" || r.URL.RawQuery != "output_modalities=decisions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		if got, want := r.Header.Get("Authorization"), fmt.Sprintf("Bearer test-token-%d", calls); got != want {
			t.Errorf("authorization = %q, want %q", got, want)
		}
		fmt.Fprint(w, `{"data":[{"id":"typesafe/jev-1.14"},{"id":"~typesafe/jev-latest"},{"id":"typesafe/jev-1.13"},{"id":"typesafe/jev-1.14"},{"id":"typesafe/jev-router"},{"id":"other/jev-1.14"},{"id":"typesafe/not-jev"},{"id":""}]}`)
	}))
	defer srv.Close()
	want := []string{"typesafe/jev-1.13", "typesafe/jev-1.14", "~typesafe/jev-latest"}
	for range 2 {
		got, err := fetchJevModels(context.Background(), srv.URL+"/api/v1/", source, srv.Client())
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("models = %v, error = %v; want %v", got, err, want)
		}
	}
	if calls != 2 {
		t.Fatalf("token calls = %d, want 2", calls)
	}
}

func TestFetchJevModelsErrors(t *testing.T) {
	const secret = "sensitive-test-token"
	source := jevTestTokenSource(func(context.Context) (string, error) { return secret, nil })
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, secret, "HTTP 401"},
		{"server error", http.StatusInternalServerError, secret, "HTTP 500"},
		{"invalid JSON", http.StatusOK, "{", "invalid OpenRouter model response"},
		{"missing data", http.StatusOK, `{}`, "invalid OpenRouter model response"},
		{"null data", http.StatusOK, `{"data":null}`, "invalid OpenRouter model response"},
		{"wrong data type", http.StatusOK, `{"data":{}}`, "invalid OpenRouter model response"},
		{"empty catalog", http.StatusOK, `{"data":[]}`, "no Jev models"},
		{"no Jev", http.StatusOK, `{"data":[{"id":"other/model"},{"id":"typesafe/jev-router"}]}`, "no Jev models"},
		{"oversized", http.StatusOK, strings.Repeat(" ", (2<<20)+1), "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			models, err := fetchJevModels(context.Background(), srv.URL+"/api/v1", source, srv.Client())
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), secret) || len(models) != 0 {
				t.Fatalf("models = %v, error = %v; want safe error containing %q", models, err, tc.want)
			}
		})
	}
}

func TestFetchJevModelsRedirectDoesNotLeakToken(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	client := srv.Client()
	source := jevTestTokenSource(func(context.Context) (string, error) { return "secret", nil })
	_, err := fetchJevModels(context.Background(), srv.URL+"/api/v1", source, client)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") || redirected {
		t.Fatalf("redirected = %v, error = %v", redirected, err)
	}
	if client.CheckRedirect != nil {
		t.Fatal("modified caller's client")
	}
}

func TestFetchJevModelsAuthAndContextErrors(t *testing.T) {
	const secret = "sensitive-test-token"
	valid := jevTestTokenSource(func(context.Context) (string, error) { return secret, nil })
	failed := jevTestTokenSource(func(context.Context) (string, error) { return "", errors.New(secret) })
	empty := jevTestTokenSource(func(context.Context) (string, error) { return " ", nil })
	invalid := jevTestTokenSource(func(context.Context) (string, error) { return "token\r\nheader", nil })
	for _, tc := range []struct {
		name, base, want string
		source           authSource
	}{
		{"invalid base", "://bad", "invalid OpenRouter API base URL", valid},
		{"userinfo", "https://user:password@example.com/api/v1", "invalid OpenRouter API base URL", valid},
		{"query", "https://example.com/api/v1?key=secret", "invalid OpenRouter API base URL", valid},
		{"no source", "http://example.com/api/v1", "authentication is required", nil},
		{"source failure", "http://example.com/api/v1", "authentication failed", failed},
		{"empty token", "http://example.com/api/v1", "invalid OpenRouter authentication token", empty},
		{"invalid token", "http://example.com/api/v1", "invalid OpenRouter authentication token", invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetchJevModels(context.Background(), tc.base, tc.source, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("error = %v, want safe error containing %q", err, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fetchJevModels(ctx, "http://127.0.0.1/api/v1", valid, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
}

func TestFetchJevModelsPreservesLoginRequired(t *testing.T) {
	source := jevTestTokenSource(func(context.Context) (string, error) { return "", ErrOIDCBrowserLoginRequired })
	_, err := fetchJevModels(context.Background(), "https://openrouter.ai/api/v1", source, nil)
	if !errors.Is(err, ErrOIDCBrowserLoginRequired) {
		t.Fatalf("login guidance lost: %v", err)
	}
}

func TestFetchJevModelsReadFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()
	source := jevTestTokenSource(func(context.Context) (string, error) { return "secret", nil })
	_, err := fetchJevModels(context.Background(), srv.URL+"/api/v1", source, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "read OpenRouter model response failed") {
		t.Fatalf("error = %v, want response read failure", err)
	}
}

func TestFetchJevModelsCanceledDuringResponse(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := jevTestTokenSource(func(context.Context) (string, error) { return "secret", nil })
	done := make(chan error, 1)
	go func() {
		_, err := fetchJevModels(ctx, srv.URL+"/api/v1", source, srv.Client())
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled response: %v", err)
	}
}
