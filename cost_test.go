package main

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
)

func costPtr[T any](v T) *T { return &v }

func TestCostReportsKnownUnknownAndCached(t *testing.T) {
	var a costAccumulator
	a.recordCachedJob()
	if s := a.snapshot(); s.costStatus != "no_calls" || s.cachedJobs != 1 {
		t.Fatalf("cached: %+v", s)
	}
	usage := &chatUsage{PromptTokens: costPtr(int64(20)), CompletionTokens: costPtr(int64(3)), Cost: costPtr(0.002)}
	r := a.recordChat(usage, true)
	if r.costSource != "provider" || r.costUSD == nil || *r.costUSD != 0.002 {
		t.Fatalf("reported: %+v", r)
	}
	a.recordChat(nil, true)
	s := a.snapshot()
	if s.costStatus != "partial" || s.requests != 2 || s.unknownCosts != 1 || s.knownUSD != 0.002 || s.input.knownTokens != 20 || s.input.missing != 1 {
		t.Fatalf("partial: %+v", s)
	}
	if text := formatCost(s); !strings.Contains(text, "provider-reported subtotal") || !strings.Contains(text, "1 requests unknown") {
		t.Fatalf("summary: %q", text)
	}
}

func TestCostRejectsUntrustedAndInvalidValues(t *testing.T) {
	for _, value := range []float64{-1, math.NaN(), math.Inf(1)} {
		var a costAccumulator
		if r := a.recordSystemOne(&systemOneUsage{Cost: &value}, true); r.costSource != "unknown" {
			t.Fatalf("invalid cost: %+v", r)
		}
	}
	var a costAccumulator
	if r := a.recordChat(&chatUsage{Cost: costPtr(1.0)}, false); r.costSource != "unknown" {
		t.Fatalf("untrusted cost: %+v", r)
	}
	if text := formatCost(a.snapshot()); !strings.Contains(strings.ToLower(text), "unknown") {
		t.Fatalf("unknown cost: %q", text)
	}
}

func TestCostSystemOneUsageAndSmallAmount(t *testing.T) {
	var response struct {
		Usage *systemOneUsage `json:"usage"`
	}
	body := `{"usage":{"input_tokens":476,"output_tokens":70,"input_tokens_details":{"cached_tokens":120},"cost":0.00000001}}`
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	var a costAccumulator
	r := a.recordSystemOne(response.Usage, true)
	if r.input.value != 476 || r.output.value != 70 || !r.cacheRead.known || r.cacheRead.value != 120 || r.costSource != "provider" {
		t.Fatalf("usage: %+v", r)
	}
	if text := formatCost(a.snapshot()); strings.Contains(text, "$0.000000") || !strings.Contains(text, "1e-08") {
		t.Fatalf("small cost: %q", text)
	}
}

func TestCostContradictoryCacheCountsAreUnknown(t *testing.T) {
	var usage chatUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":90,"cache_write_tokens":11},"cost":0.25}`), &usage); err != nil {
		t.Fatal(err)
	}
	var a costAccumulator
	r := a.recordChat(&usage, true)
	if r.cacheRead.known || r.cacheWrite.known || r.costSource != "provider" {
		t.Fatalf("cache inconsistency: %+v", r)
	}
}

func TestCostAccumulatorConcurrentAndOverflow(t *testing.T) {
	var a costAccumulator
	const workers, calls = 32, 100
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range calls {
				a.recordSystemOne(&systemOneUsage{InputTokens: costPtr(int64(2)), Cost: costPtr(0.5)}, true)
				a.recordCachedJob()
				_ = a.snapshot()
			}
		})
	}
	wg.Wait()
	s := a.snapshot()
	if s.requests != workers*calls || s.cachedJobs != workers*calls || s.costStatus != "exact" || s.knownUSD != workers*calls*0.5 {
		t.Fatalf("totals: %+v", s)
	}
	var overflow costAccumulator
	overflow.recordSystemOne(&systemOneUsage{InputTokens: costPtr(int64(math.MaxInt64)), Cost: costPtr(math.MaxFloat64)}, true)
	overflow.recordSystemOne(&systemOneUsage{InputTokens: costPtr(int64(1)), Cost: costPtr(math.MaxFloat64)}, true)
	s = overflow.snapshot()
	if s.costStatus != "partial" || s.input.missing != 1 {
		t.Fatalf("overflow: %+v", s)
	}
}
