package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/zalando/go-keyring"
)

const apiKeyKeychainService = "lintdec"

func storeAPIKey(key string, set func(service, account, secret string) error) (string, error) {
	if key == "" {
		return "", errors.New("API key is empty")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("could not generate API key reference")
	}
	ref := hex.EncodeToString(random[:])
	if set == nil {
		set = keyring.Set
	}
	if err := set(apiKeyKeychainService, ref, key); err != nil {
		return ref, errors.New("could not store API key in keychain")
	}
	return ref, nil
}

func prepareProviderAPIKey(p *Provider, set func(string, string, string) error) (string, error) {
	if p.APIKey == "" {
		return "", nil
	}
	ref, err := storeAPIKey(p.APIKey, set)
	if err != nil {
		return ref, err
	}
	p.APIKey, p.APIKeyRef, p.APIKeyBaseURL = "", ref, p.BaseURL
	return ref, nil
}

func deleteUnusedAPIKey(c Config, p Provider, remove func(string, string) error) error {
	if p.APIKeyRef == "" {
		return nil
	}
	for _, other := range c.Providers {
		if other.APIKeyRef == p.APIKeyRef {
			return nil
		}
	}
	err := remove(apiKeyKeychainService, p.APIKeyRef)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

type keychainAPIKeyResult struct {
	done chan struct{}
	key  string
	err  error
}

type keychainAPIKeySource struct {
	ref string
	get func(service, account string) (string, error)

	mu      sync.Mutex
	key     string
	loading *keychainAPIKeyResult
}

func (s *keychainAPIKeySource) token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.ref == "" {
		return "", errors.New("API key reference is empty")
	}

	s.mu.Lock()
	if s.key != "" {
		key := s.key
		s.mu.Unlock()
		return key, nil
	}
	result := s.loading
	if result == nil {
		result = &keychainAPIKeyResult{done: make(chan struct{})}
		s.loading = result
		get := s.get
		if get == nil {
			get = keyring.Get
		}
		go func() {
			key, err := get(apiKeyKeychainService, s.ref)
			if err != nil || key == "" {
				key, err = "", errors.New("could not retrieve API key from keychain")
			}
			s.mu.Lock()
			result.key, result.err = key, err
			if err == nil {
				s.key = key
			}
			s.loading = nil
			close(result.done)
			s.mu.Unlock()
		}()
	}
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-result.done:
		return result.key, result.err
	}
}
