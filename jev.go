package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// fetchJevModels expects the OpenRouter API base URL (for example, https://openrouter.ai/api/v1).
// It obtains a fresh token from source for each discovery and does not follow redirects.
func fetchJevModels(ctx context.Context, baseURL string, source authSource, httpc *http.Client) ([]string, error) {
	base, err := url.Parse(baseURL)
	if err != nil || !discoveryBase(base) {
		return nil, fmt.Errorf("invalid OpenRouter API base URL")
	}
	if source == nil {
		return nil, fmt.Errorf("OpenRouter authentication is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	token, err := discoveryToken(ctx, source)
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/models"
	base.RawPath = ""
	base.RawQuery = "output_modalities=decisions"
	body, err := getCatalog(ctx, base.String(), token, httpc)
	if err != nil {
		return nil, err
	}
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &catalog) != nil || catalog.Data == nil {
		return nil, fmt.Errorf("invalid OpenRouter model response")
	}
	var ids []string
	for _, model := range catalog.Data {
		if jevModel(model.ID) {
			ids = append(ids, model.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no Jev models found in OpenRouter catalog")
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

func discoveryBase(u *url.URL) bool {
	return u != nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// jevModel: a versioned Jev model; the router alias is not a model to pin.
func jevModel(id string) bool {
	name, ok := strings.CutPrefix(id, "typesafe/jev-")
	if !ok {
		name, ok = strings.CutPrefix(id, "~typesafe/jev-")
	}
	return ok && name != "" && !strings.HasPrefix(name, "router")
}

// discoveryToken: a bearer token safe to put in a header, with errors that never echo it.
func discoveryToken(ctx context.Context, source authSource) (string, error) {
	token, err := source.token(ctx)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return "", fmt.Errorf("OpenRouter authentication: %w", ctx.Err())
	case errors.Is(err, ErrOIDCBrowserLoginRequired):
		return "", err
	default:
		return "", fmt.Errorf("OpenRouter authentication failed")
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("invalid OpenRouter authentication token")
	}
	return token, nil
}

// getCatalog fetches the model list without following redirects, capped at 2 MiB.
func getCatalog(ctx context.Context, rawURL, token string, httpc *http.Client) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid OpenRouter API base URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if httpc == nil {
		httpc = http.DefaultClient
	}
	client := *httpc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("OpenRouter model discovery: %w", ctx.Err())
		}
		return nil, fmt.Errorf("OpenRouter model discovery request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenRouter model discovery: HTTP %d", res.StatusCode)
	}
	const maxResponse = 2 << 20
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("read OpenRouter model response: %w", ctx.Err())
		}
		return nil, fmt.Errorf("read OpenRouter model response failed")
	}
	if len(body) > maxResponse {
		return nil, fmt.Errorf("OpenRouter model response exceeds %d bytes", maxResponse)
	}
	return body, nil
}
