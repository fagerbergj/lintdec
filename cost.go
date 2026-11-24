package main

import (
	"fmt"
	"math"
	"sync"
)

// chatUsage and systemOneUsage are the usage objects in non-streaming responses.
// Pointers distinguish omitted values from reported zeroes.
type chatUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens *int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	Cost *float64 `json:"cost"`
}

type systemOneUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	InputDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	Cost *float64 `json:"cost"`
}

type tokenValue struct {
	value int64
	known bool
}

// Input is the provider-reported count; chat input includes cache reads and writes.
type requestAccounting struct {
	input, output, cacheRead, cacheWrite tokenValue
	costUSD                              *float64 // nil when unknown
	costSource                           string   // "provider" or "unknown"
}

type tokenTotal struct {
	knownTokens int64
	missing     uint64
}

// knownUSD and knownTokens are subtotals; costStatus and missing determine completeness.
type costSummary struct {
	requests, cachedJobs, providerCosts, unknownCosts uint64
	knownUSD                                          float64
	costStatus                                        string // "no_calls", "exact", "partial", or "unknown"
	input, output, cacheRead, cacheWrite              tokenTotal
}

// costAccumulator stores only totals, not an unbounded list of requests. Its zero value is ready to use.
type costAccumulator struct {
	mu sync.Mutex
	s  costSummary
}

func tokens(v *int64) tokenValue {
	if v == nil || *v < 0 {
		return tokenValue{}
	}
	return tokenValue{value: *v, known: true}
}

func validUSD(v *float64) bool {
	return v != nil && *v >= 0 && !math.IsInf(*v, 0) && !math.IsNaN(*v)
}

func (a *costAccumulator) recordChat(u *chatUsage, trustCost bool) requestAccounting {
	r := requestAccounting{costSource: "unknown"}
	if u != nil {
		r.input, r.output = tokens(u.PromptTokens), tokens(u.CompletionTokens)
		if u.PromptDetails != nil {
			r.cacheRead, r.cacheWrite = tokens(u.PromptDetails.CachedTokens), tokens(u.PromptDetails.CacheWriteTokens)
		}
		if r.input.known && (r.cacheRead.value > r.input.value || r.cacheWrite.value > r.input.value-r.cacheRead.value) {
			r.cacheRead, r.cacheWrite = tokenValue{}, tokenValue{}
		}
		if trustCost && validUSD(u.Cost) {
			usd := *u.Cost
			r.costUSD, r.costSource = &usd, "provider"
		}
	}
	a.record(r)
	return r
}

// recordSystemOne uses documented usage counts and a trusted provider's usage.cost;
// no System One cache-write or model-price semantics are assumed.
func (a *costAccumulator) recordSystemOne(u *systemOneUsage, trustCost bool) requestAccounting {
	r := requestAccounting{costSource: "unknown"}
	if u != nil {
		r.input, r.output = tokens(u.InputTokens), tokens(u.OutputTokens)
		if u.InputDetails != nil {
			r.cacheRead = tokens(u.InputDetails.CachedTokens)
			if r.input.known && r.cacheRead.known && r.cacheRead.value > r.input.value {
				r.cacheRead = tokenValue{}
			}
		}
		if trustCost && validUSD(u.Cost) {
			usd := *u.Cost
			r.costUSD, r.costSource = &usd, "provider"
		}
	}
	a.record(r)
	return r
}

func addTokens(total *tokenTotal, value tokenValue) {
	if !value.known || value.value > math.MaxInt64-total.knownTokens {
		total.missing++
		return
	}
	total.knownTokens += value.value
}

func (a *costAccumulator) record(r requestAccounting) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &a.s
	s.requests++
	addTokens(&s.input, r.input)
	addTokens(&s.output, r.output)
	addTokens(&s.cacheRead, r.cacheRead)
	addTokens(&s.cacheWrite, r.cacheWrite)
	if r.costSource != "unknown" && validUSD(r.costUSD) && validUSDValue(s.knownUSD+*r.costUSD) {
		s.knownUSD += *r.costUSD
		s.providerCosts++
	} else {
		s.unknownCosts++
	}
}

func validUSDValue(v float64) bool { return validUSD(&v) }

// recordCachedJob counts a fully cached job; partially cached jobs still make a request.
func (a *costAccumulator) recordCachedJob() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.s.cachedJobs++
}

func formatCost(s costSummary) string {
	amount := fmt.Sprintf("%.6f", s.knownUSD)
	if s.knownUSD > 0 && amount == "0.000000" {
		amount = fmt.Sprintf("%.6g", s.knownUSD)
	}
	switch s.costStatus {
	case "no_calls":
		return "Cost: no model calls"
	case "unknown":
		return fmt.Sprintf("Cost: unknown (%d requests)", s.requests)
	case "partial":
		return fmt.Sprintf("Cost: $%s provider-reported subtotal (USD); %d requests unknown", amount, s.unknownCosts)
	default:
		return fmt.Sprintf("Cost: $%s provider-reported (USD; other charges may apply)", amount)
	}
}

func (a *costAccumulator) snapshot() costSummary {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.s
	switch {
	case s.requests == 0:
		s.costStatus = "no_calls"
	case s.unknownCosts == s.requests:
		s.costStatus = "unknown"
	case s.unknownCosts > 0:
		s.costStatus = "partial"
	default:
		s.costStatus = "exact"
	}
	return s
}
