package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var _ authSource = (*keychainAPIKeySource)(nil)

func TestStoreAPIKey(t *testing.T) {
	const key = "secret-api-key"
	stored := make(map[string]string)
	set := func(service, account, secret string) error {
		if service != apiKeyKeychainService || secret != key {
			t.Errorf("unexpected keychain service or secret")
		}
		stored[account] = secret
		return nil
	}
	first, err := storeAPIKey(key, set)
	if err != nil {
		t.Fatal(err)
	}
	second, err := storeAPIKey(key, set)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{first, second} {
		decoded, err := hex.DecodeString(ref)
		if err != nil || len(decoded) != 32 || strings.Contains(ref, key) || stored[ref] != key {
			t.Errorf("invalid opaque reference %q", ref)
		}
	}
	if first == second {
		t.Fatal("references must be distinct")
	}
}

func TestStoreAPIKeyRejectsEmptyAndRedactsErrors(t *testing.T) {
	const secret = "secret-api-key"
	calls := 0
	set := func(service, account, key string) error {
		calls++
		return fmt.Errorf("keychain leaked %s", secret)
	}
	if ref, err := storeAPIKey("", set); err == nil || ref != "" || calls != 0 {
		t.Fatalf("empty key: ref=%q err=%v calls=%d", ref, err, calls)
	}
	ref, err := storeAPIKey(secret, set)
	if err == nil || ref == "" || calls != 1 || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe storage error: ref=%q err=%v calls=%d", ref, err, calls)
	}
}

func TestPrepareProviderAPIKeyAndCleanup(t *testing.T) {
	const secret = "secret-api-key"
	p := Provider{BaseURL: "https://api.example/v1", APIKey: secret}
	ref, err := prepareProviderAPIKey(&p, func(service, account, key string) error {
		if service != apiKeyKeychainService || key != secret {
			t.Fatal("wrong credential")
		}
		return nil
	})
	if err != nil || ref == "" || p.APIKey != "" || p.APIKeyRef != ref || p.APIKeyBaseURL != p.BaseURL {
		t.Fatalf("provider key not protected: %+v %v", p, err)
	}
	data, err := json.Marshal(p)
	if err != nil || strings.Contains(string(data), secret) {
		t.Fatalf("key leaked into config: %v", err)
	}
	calls := 0
	remove := func(service, account string) error {
		calls++
		if account != ref {
			t.Fatal("wrong key removed")
		}
		return nil
	}
	if err := deleteUnusedAPIKey(Config{Providers: map[string]Provider{"shared": p}}, p, remove); err != nil || calls != 0 {
		t.Fatal("removed shared credential")
	}
	if err := deleteUnusedAPIKey(Config{Providers: map[string]Provider{}}, p, remove); err != nil || calls != 1 {
		t.Fatal("unused credential not removed")
	}
}

func TestKeychainAPIKeySourceCachesConcurrentGets(t *testing.T) {
	const ref, key = "reference", "secret-api-key"
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	source := &keychainAPIKeySource{ref: ref, get: func(service, account string) (string, error) {
		if service != apiKeyKeychainService || account != ref {
			t.Errorf("unexpected keychain service or account")
		}
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return key, nil
	}}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := source.token(context.Background())
			if err != nil || got != key {
				t.Errorf("unexpected token or error: %v", err)
			}
		}()
	}
	<-started
	unblock()
	wg.Wait()
	if got, err := source.token(context.Background()); err != nil || got != key || calls.Load() != 1 {
		t.Fatalf("cache not used: err=%v calls=%d", err, calls.Load())
	}
}

func TestKeychainAPIKeySourceErrorsAreRedactedAndRetried(t *testing.T) {
	const secret = "secret-api-key"
	calls := 0
	source := &keychainAPIKeySource{ref: "reference", get: func(service, account string) (string, error) {
		calls++
		switch calls {
		case 1:
			return secret, fmt.Errorf("keychain leaked %s", secret)
		case 2:
			return "", nil
		default:
			return secret, nil
		}
	}}
	for range 2 {
		got, err := source.token(context.Background())
		if err == nil || got != "" || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe retrieval error: key=%q err=%v", got, err)
		}
	}
	if got, err := source.token(context.Background()); err != nil || got != secret || calls != 3 {
		t.Fatalf("retry failed: err=%v calls=%d", err, calls)
	}
}

func TestKeychainAPIKeySourceCancellationWhileGetBlocked(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	source := &keychainAPIKeySource{ref: "reference", get: func(service, account string) (string, error) {
		calls.Add(1)
		close(started)
		<-release
		return "secret-api-key", nil
	}}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstResult := make(chan error, 1)
	go func() {
		_, err := source.token(firstCtx)
		firstResult <- err
	}()
	<-started

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWait()
	if _, err := source.token(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting caller: expected deadline exceeded, got %v", err)
	}
	cancelFirst()
	select {
	case err := <-firstResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first caller: expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first caller did not stop while keychain Get was blocked")
	}
	unblock()
	if got, err := source.token(context.Background()); err != nil || got != "secret-api-key" || calls.Load() != 1 {
		t.Fatalf("blocked get was not shared: err=%v calls=%d", err, calls.Load())
	}
}

func TestKeychainAPIKeySourceRejectsEmptyReferenceAndCanceledContext(t *testing.T) {
	get := func(service, account string) (string, error) {
		t.Fatal("unexpected keychain Get")
		return "", nil
	}
	if _, err := (&keychainAPIKeySource{get: get}).token(context.Background()); err == nil {
		t.Fatal("empty reference accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&keychainAPIKeySource{ref: "reference", get: get}).token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
