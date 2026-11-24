package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestClientReportsProviderCostAndRetries(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		content := `{"comment-long":{"lint":false}}`
		if n > 1 {
			content = `{"todo-no-owner":{"lint":false}}`
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "test-model", "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 4, "cost": 0.002}, "choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	}))
	defer srv.Close()
	var totals costAccumulator
	r1, _ := findRule("comment-long")
	r2, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, model: "test-model", httpc: srv.Client(), costs: &totals, trustCost: true}
	_, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{r1, r2}})
	if err != nil {
		t.Fatal(err)
	}
	s := totals.snapshot()
	if s.requests != 2 || s.costStatus != "exact" || s.knownUSD != 0.004 {
		t.Fatalf("incorrect totals: %+v", s)
	}
}

func TestProxyCostUnknownWithoutTierEvidence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "test-model", "cost": map[string]any{"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5}}}})
		case "/chat/completions":
			json.NewEncoder(w).Encode(map[string]any{"model": "test-model", "usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 100, "prompt_tokens_details": map[string]any{"cached_tokens": 200, "cache_write_tokens": 0}}, "choices": []any{map[string]any{"message": map[string]any{"content": `{"todo-no-owner":{"lint":false}}`}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var totals costAccumulator
	rule, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, model: "test-model", httpc: srv.Client(), costs: &totals}
	if _, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	s := totals.snapshot()
	if s.costStatus != "unknown" || s.input.knownTokens != 1000 || s.cacheRead.knownTokens != 200 {
		t.Fatalf("unverified tier: %+v", s)
	}
}

func TestSystemOneClientReportsProviderCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"todo-no-owner": map[string]any{"noul": 0.9}}, "usage": map[string]any{"input_tokens": 80, "output_tokens": 2, "input_tokens_details": map[string]any{"cached_tokens": 10}, "cost": 0.00003}})
	}))
	defer srv.Close()
	var totals costAccumulator
	rule, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, protocol: "systemone", model: "typesafe/jev-1.14", httpc: srv.Client(), costs: &totals, trustCost: true}
	if _, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	s := totals.snapshot()
	if s.costStatus != "exact" || s.knownUSD != 0.00003 || s.input.knownTokens != 80 || s.cacheRead.knownTokens != 10 || s.cacheRead.missing != 0 {
		t.Fatalf("incorrect cost: %+v", s)
	}
}

func TestCostSummaryDoesNotClaimUnknownIsFree(t *testing.T) {
	var totals costAccumulator
	totals.recordChat(nil, false)
	text := formatCost(totals.snapshot())
	if !strings.Contains(strings.ToLower(text), "unknown") || strings.Contains(text, "$0.00") {
		t.Fatalf("misleading summary: %q", text)
	}
}

func TestCostTracksRetryAndAllFollowups(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch count.Add(1) {
		case 1:
			http.Error(w, "temporary failure", http.StatusInternalServerError)
		case 2:
			json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"cost": 0.001}, "choices": []any{map[string]any{"message": map[string]any{"content": `{"comment-long":{"lint":false}}`}}}})
		case 3:
			json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"cost": 0.002}, "choices": []any{map[string]any{"message": map[string]any{"content": `{}`}}}})
		case 4:
			json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"cost": 0.003}, "choices": []any{map[string]any{"message": map[string]any{"content": `{"todo-no-owner":{"lint":false}}`}}}})
		default:
			t.Error("unexpected model request")
		}
	}))
	defer srv.Close()
	var totals costAccumulator
	r1, _ := findRule("comment-long")
	r2, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, model: "test-model", httpc: srv.Client(), costs: &totals, trustCost: true}
	verdicts, err := runWithRetry(context.Background(), cl, job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{r1, r2}})
	if err != nil || len(verdicts) != 2 {
		t.Fatalf("verdicts=%v err=%v", verdicts, err)
	}
	s := totals.snapshot()
	if count.Load() != 4 || s.requests != 4 || s.providerCosts != 3 || s.unknownCosts != 1 || s.costStatus != "partial" || s.knownUSD < 0.005999 || s.knownUSD > 0.006001 {
		t.Fatalf("retry/follow-up totals: %+v", s)
	}
}

func TestCostCacheOnlyAndPartiallyCachedJobs(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"cost": 0.001}, "choices": []any{map[string]any{"message": map[string]any{"content": `{"todo-no-owner":{"lint":false}}`}}}})
	}))
	defer srv.Close()
	var totals costAccumulator
	r1, _ := findRule("comment-long")
	r2, _ := findRule("todo-no-owner")
	u := Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}
	cl := &client{base: srv.URL, model: "test-model", httpc: srv.Client(), costs: &totals, trustCost: true}
	ch := &cache{dir: t.TempDir(), on: true}
	ch.put(ch.key(cl.cacheModel(), "0", r1.ID, u), Verdict{Lint: false})
	opts := lintOpts{workers: 1}
	runPool(context.Background(), []job{{unit: u, rules: []Rule{r1}}}, opts, cl, ch, newProgress(1, true), nil, nil)
	if s := totals.snapshot(); s.costStatus != "no_calls" || s.cachedJobs != 1 || calls.Load() != 0 {
		t.Fatalf("fully cached: %+v calls=%d", s, calls.Load())
	}
	runPool(context.Background(), []job{{unit: u, rules: []Rule{r1, r2}}}, opts, cl, ch, newProgress(1, true), nil, nil)
	if s := totals.snapshot(); s.requests != 1 || s.cachedJobs != 1 || s.costStatus != "exact" || s.knownUSD != 0.001 || calls.Load() != 1 {
		t.Fatalf("partially cached: %+v calls=%d", s, calls.Load())
	}
}

func TestProxyProviderCostWinsWithoutTierEvidence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "test-model", "usage": map[string]any{"prompt_tokens": 150_000, "completion_tokens": 1, "cost": 0.004}, "choices": []any{map[string]any{"message": map[string]any{"content": `{"todo-no-owner":{"lint":false}}`}}}})
	}))
	defer srv.Close()
	var totals costAccumulator
	rule, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, model: "test-model", httpc: srv.Client(), costs: &totals, trustCost: true}
	if _, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	if s := totals.snapshot(); s.costStatus != "exact" || s.knownUSD != 0.004 {
		t.Fatalf("provider cost ignored: %+v", s)
	}
}

func TestConcurrentProxyCallsAccountOnceEach(t *testing.T) {
	var modelCalls, catalogCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			catalogCalls.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "test-model", "cost": map[string]any{"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5}}}})
			return
		}
		modelCalls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"model": "test-model", "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "cost": 0.5}, "choices": []any{map[string]any{"message": map[string]any{"content": `{"todo-no-owner":{"lint":false}}`}}}})
	}))
	defer srv.Close()
	var totals costAccumulator
	rule, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, model: "test-model", httpc: srv.Client(), costs: &totals, trustCost: true}
	j := job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}}
	const calls = 16
	var wg sync.WaitGroup
	for range calls {
		wg.Go(func() {
			if _, err := cl.call(context.Background(), j); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if s := totals.snapshot(); s.requests != calls || s.costStatus != "exact" || s.knownUSD != calls*0.5 || modelCalls.Load() != calls || catalogCalls.Load() > 1 {
		t.Fatalf("concurrent totals: %+v model=%d catalog=%d", s, modelCalls.Load(), catalogCalls.Load())
	}
}
