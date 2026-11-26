package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

type client struct {
	base         string
	protocol     string // ""|"openai" or "systemone"
	model        string
	apiKey       string
	source       authSource
	trustCost    bool
	costs        *costAccumulator
	budget       *requestBudget
	temp         float64
	threshold    float64
	thresholdSet bool
	calibrate    bool
	httpc        *http.Client
}

var errBudgetExhausted = errors.New("model request budget exhausted")

type requestBudget struct {
	limit int64
	used  atomic.Int64
}

func (b *requestBudget) take() bool {
	if b == nil || b.limit <= 0 {
		return true
	}
	for {
		used := b.used.Load()
		if used >= b.limit {
			return false
		}
		if b.used.CompareAndSwap(used, used+1) {
			return true
		}
	}
}

func (c *client) cacheModel() string {
	return c.base + "|" + c.protocol + "|" + c.model
}

func (c *client) cacheTemp() string {
	if c.protocol == "systemone" {
		return "systemone-1q"
	}
	return fmt.Sprintf("%v", c.temp)
}

func (c *client) cacheRuleVersion(ruleID string) string {
	if c.protocol == "systemone" {
		return ruleVersion(ruleID)
	}
	return allRulesVersion
}

func (c *client) floorFor(r Rule) float64 {
	if c.thresholdSet {
		return c.threshold
	}
	return ruleFloor(r.ID)
}

type Verdict struct {
	Lint       bool     `json:"lint"`
	Severity   string   `json:"severity"`
	Confidence float64  `json:"confidence"`
	Line       *int     `json:"line"`
	Reason     string   `json:"reason"`
	Veto       *float64 `json:"veto,omitempty"` // probability the rule's exception applies
}

const systemPrompt = `You are a strict code linter. Each call gives you one TARGET and a numbered list of rules.
Decide each rule independently against the TARGET, then reply with exactly one JSON object and nothing else, no markdown fences.
The object maps every rule id to its verdict — the key MUST be the rule id exactly as written (for example "comment-long"), never the rule number:
{"rule-id": {"lint": true|false, "severity": "low"|"med"|"high", "confidence": <0.0 to 1.0>, "line": <line number where the violation starts, or null>, "reason": "<at most 12 words>"}, ...}
For a clean rule use {"lint": false} and nothing else.
confidence is your certainty that the lint is real: below 0.5 = unsure (then lint is false), 0.5-0.8 = probably, above 0.8 = sure.
Rules: set lint true only when you are confident. severity: low = style, med = should fix, high = likely wrong.
line is an absolute line number inside the TARGET (null if the whole unit is the violation).
If the TARGET is a numbered digest, line must be one of the printed line numbers.`

type err429 struct{ msg string }

func (e err429) Error() string { return e.msg }

// call asks the model for one batch: every rule in the job against the unit.
// OpenAI gets a follow-up for omitted rules; systemone answers all in one pass.
func (c *client) call(ctx context.Context, j job) (map[string]Verdict, error) {
	if c.protocol == "systemone" {
		return c.callSystemOne(ctx, j)
	}
	out, missing, err := c.callOnce(ctx, j, "")
	if err != nil {
		return nil, err
	}
	// One follow-up pass for omitted rules only; anything still omitted gets a
	// fresh single-rule call, which models handle reliably.
	if len(missing) > 0 {
		var missRules []Rule
		for _, id := range missing {
			r, ok := findRule(id)
			if !ok {
				return nil, fmt.Errorf("unknown rule %s", id)
			}
			missRules = append(missRules, r)
		}
		fu := "Your previous reply was missing verdicts for: " + strings.Join(missing, ", ") + ". Reply now with exactly one JSON object containing only those rule ids as keys, in the same verdict format."
		out2, missing2, err := c.callOnce(ctx, job{unit: j.unit, rules: missRules}, fu)
		if err != nil {
			return nil, err
		}
		maps.Copy(out, out2)
		for _, id := range missing2 {
			r, ok := findRule(id)
			if !ok {
				return nil, fmt.Errorf("unknown rule %s", id)
			}
			out3, missing3, err := c.callOnce(ctx, job{unit: j.unit, rules: []Rule{r}}, "")
			if err != nil {
				return nil, err
			}
			if len(missing3) > 0 {
				return nil, fmt.Errorf("model omitted %s even alone", id)
			}
			out[id] = out3[id]
		}
	}
	return out, nil
}

// callSystemOne sends one rule (+ its veto) per request against the shared state
// so sibling rubrics cannot suppress each other and cache entries stay independent.
func (c *client) callSystemOne(ctx context.Context, j job) (map[string]Verdict, error) {
	var state strings.Builder
	fmt.Fprintf(&state, "FILE: %s\nUNIT: %s, lines %d-%d\n", j.unit.File, j.unit.Kind, j.unit.Start, j.unit.End)
	if j.unit.Context != "" {
		fmt.Fprintf(&state, "SURROUNDING CONTEXT (reference only, do not lint it):\n%s\n", j.unit.Context)
	}
	fmt.Fprintf(&state, "TARGET:\n%s", j.unit.Text)
	stateStr := state.String()

	out := make(map[string]Verdict, len(j.rules))
	for _, r := range j.rules {
		v, err := c.callSystemOneRule(ctx, stateStr, r)
		if err != nil {
			return out, err
		}
		out[r.ID] = v
	}
	return out, nil
}

func (c *client) callSystemOneRule(ctx context.Context, state string, r Rule) (Verdict, error) {
	qs := withVetoes([]Rule{r})
	payload, _ := json.Marshal(map[string]any{
		"model":     c.model,
		"state":     state,
		"questions": systemOneQuestions(qs),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", systemOneRoot(c.base)+"/v1/systemone", bytes.NewReader(payload))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := authorizeRequest(req, c.source, c.apiKey); err != nil {
		return Verdict{}, err
	}
	if !c.budget.take() {
		return Verdict{}, errBudgetExhausted
	}
	var usage *systemOneUsage
	if c.costs != nil {
		defer func() { c.costs.recordSystemOne(usage, c.trustCost) }()
	}
	res, err := sendRequest(c.httpc, req, c.source != nil || c.apiKey != "")
	if err != nil {
		return Verdict{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable {
		return Verdict{}, err429{responseError(res, c.source, c.apiKey)}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return Verdict{}, errors.New(responseError(res, c.source, c.apiKey))
	}
	var resp struct {
		Answers map[string]struct {
			Noul *float64 `json:"noul"`
		} `json:"answers"`
		Usage *systemOneUsage `json:"usage"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return Verdict{}, err
	}
	usage = resp.Usage
	probs := make(map[string]*float64, len(resp.Answers))
	for id, a := range resp.Answers {
		probs[id] = a.Noul
	}
	v, err := systemOneVerdicts(qs, probs, c.threshold)
	if err != nil {
		return Verdict{}, err
	}
	return attachVetoes([]Rule{r}, v)[r.ID], nil
}

// applySystemOneThreshold derives Lint and Severity from a cached or fresh noul
// probability so --threshold sweeps reuse cached verdicts without re-running.
func applySystemOneThreshold(v Verdict, thresh float64) Verdict {
	if thresh <= 0 {
		thresh = 0.75
	}
	v.Lint = v.Confidence >= thresh
	switch {
	case !v.Lint:
		v.Severity = "low"
	case v.Confidence >= 0.9:
		v.Severity = "high"
	default:
		v.Severity = "med"
	}
	return v
}

// systemOneQuestions: one yes/no question per rule; exception questions are asked as written.
func systemOneQuestions(rules []Rule) map[string]map[string]string {
	questions := make(map[string]map[string]string, len(rules))
	for _, r := range rules {
		instructions := fmt.Sprintf("Does the TARGET violate this rule? %s — %s. CRITERIA: %s", r.ID, r.Summary, r.Rubric)
		if r.Scope == "question" {
			instructions = r.Rubric
		}
		questions[r.ID] = map[string]string{"type": "noul", "instructions": instructions}
	}
	return questions
}

// systemOneVerdicts turns Jev's probabilities into verdicts; every rule needs a valid one.
func systemOneVerdicts(rules []Rule, probs map[string]*float64, thresh float64) (map[string]Verdict, error) {
	out := make(map[string]Verdict, len(rules))
	for _, r := range rules {
		p := probs[r.ID]
		if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 || *p > 1 {
			return nil, fmt.Errorf("systemone response missing or invalid probability for %s", r.ID)
		}
		out[r.ID] = applySystemOneThreshold(Verdict{Confidence: *p, Reason: r.Summary}, thresh)
	}
	return out, nil
}

// chatPrompt: the user message for one batch, the rules first and the unit last.
func chatPrompt(j job, followUp string) string {
	var sb strings.Builder
	sb.WriteString("RULES:\n")
	for i, r := range j.rules {
		fmt.Fprintf(&sb, "%d. %s — %s\n   CRITERIA: %s\n", i+1, r.ID, r.Summary, r.Rubric)
	}
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "FILE: %s\nUNIT: %s, lines %d-%d\n", j.unit.File, j.unit.Kind, j.unit.Start, j.unit.End)
	if j.unit.Context != "" {
		fmt.Fprintf(&sb, "SURROUNDING CONTEXT (reference only, do not lint it):\n%s\n", j.unit.Context)
	}
	fmt.Fprintf(&sb, "TARGET:\n%s", j.unit.Text)
	if followUp != "" {
		sb.WriteByte('\n')
		sb.WriteString(followUp)
	}
	return sb.String()
}

func (c *client) callOnce(ctx context.Context, j job, followUp string) (map[string]Verdict, []string, error) {
	body := map[string]any{
		"model": c.model, "temperature": c.temp, "max_tokens": 4096,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": chatPrompt(j, followUp)},
		},
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.base, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := authorizeRequest(req, c.source, c.apiKey); err != nil {
		return nil, nil, err
	}
	if !c.budget.take() {
		return nil, nil, errBudgetExhausted
	}
	var usage *chatUsage
	if c.costs != nil {
		defer func() { c.costs.recordChat(usage, c.trustCost) }()
	}
	res, err := sendRequest(c.httpc, req, c.source != nil || c.apiKey != "")
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, nil, err429{responseError(res, c.source, c.apiKey)}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, nil, errors.New(responseError(res, c.source, c.apiKey))
	}
	raw, err := decodeCompletion(res.Body, &usage)
	if err != nil {
		return nil, nil, err
	}
	return parseVerdicts(raw, j)
}

// decodeCompletion returns the first choice's text, falling back to reasoning-only replies.
func decodeCompletion(body io.Reader, usage **chatUsage) (string, error) {
	var out struct {
		Usage   *chatUsage `json:"usage"`
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				Reasoning *string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return "", err
	}
	*usage = out.Usage
	if len(out.Choices) == 0 {
		return "", errors.New("empty completion")
	}
	if m := out.Choices[0].Message; m.Content != nil && strings.TrimSpace(*m.Content) != "" {
		return *m.Content, nil
	} else if m.Reasoning != nil {
		return *m.Reasoning, nil
	}
	return "", nil
}

// extractVerdictMap scans valid top-level JSON objects in raw, preferring the
// last object keyed by any expected rule id over stray braces in prose.
func extractVerdictMap(raw string, rules []Rule) (map[string]json.RawMessage, error) {
	var bestMatch map[string]json.RawMessage
	sawEmpty := false
	for i := 0; i < len(raw); i++ {
		if raw[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw[i:]))
		var m map[string]json.RawMessage
		if err := dec.Decode(&m); err != nil {
			continue
		}
		if len(m) == 0 {
			sawEmpty = true
		} else if matchesAnyRule(m, rules) {
			bestMatch = m
		}
		if off := int(dec.InputOffset()); off > 1 {
			i += off - 1
		}
	}
	if bestMatch != nil {
		return bestMatch, nil
	}
	if sawEmpty {
		return map[string]json.RawMessage{}, nil
	}
	return fallbackVerdictError(raw)
}

func matchesAnyRule(m map[string]json.RawMessage, rules []Rule) bool {
	for i, r := range rules {
		if _, ok := m[r.ID]; ok {
			return true
		}
		if _, ok := m[fmt.Sprint(i+1)]; ok {
			return true
		}
	}
	return false
}

func fallbackVerdictError(raw string) (map[string]json.RawMessage, error) {
	s, e := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if s < 0 || e <= s {
		return nil, fmt.Errorf("no JSON in model reply: %.80q", raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw[s:e+1]), &m); err != nil {
		return nil, fmt.Errorf("bad model JSON: %w", err)
	}
	return nil, fmt.Errorf("model JSON has no expected rule keys: %.80q", raw[s:e+1])
}

// parseVerdicts extracts the rule-id → verdict map. It returns the verdicts
// parsed so far plus the ids the model omitted; malformed JSON is an error.
func parseVerdicts(raw string, j job) (map[string]Verdict, []string, error) {
	m, err := extractVerdictMap(raw, j.rules)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]Verdict, len(j.rules))
	var missing []string
	for i, r := range j.rules {
		b, ok := m[r.ID]
		if !ok {
			// models sometimes key by the rule number instead of the id
			b, ok = m[fmt.Sprint(i+1)]
		}
		if !ok {
			missing = append(missing, r.ID)
			continue
		}
		var v Verdict
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, nil, fmt.Errorf("bad verdict for %s: %w", r.ID, err)
		}
		out[r.ID] = normalizeVerdict(v, j.unit)
	}
	return out, missing, nil
}

func normalizeVerdict(v Verdict, u Unit) Verdict {
	if v.Line != nil && (*v.Line < u.Start || *v.Line > u.End) {
		l := u.Start
		v.Line = &l
	}
	if v.Severity != "low" && v.Severity != "med" && v.Severity != "high" {
		v.Severity = "med"
	}
	if v.Lint && v.Reason == "" {
		v.Reason = "no reason given"
	}
	if v.Confidence < 0 || v.Confidence > 1 {
		v.Confidence = 0.5
	}
	return v
}

type cache struct {
	dir string
	on  bool
}

func newCache(on bool) *cache {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = ".lintdec"
	}
	return &cache{dir: filepath.Join(dir, "lintdec", "cache"), on: on}
}

func (c *cache) key(model, tempStr, ruleID, version string, u Unit) string {
	h := sha256.New()
	abs, _ := filepath.Abs(u.File)
	// version is per-rule for systemone (1 rule per prompt) and catalogue-wide
	// for batched chat prompts, so rubric edits invalidate every affected verdict.
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s\n%s\n%s", model, tempStr, ruleID, u.Kind, version, abs, u.Text, u.Context)
	return hex.EncodeToString(h.Sum(nil))
}

func (c *cache) get(k string) (Verdict, bool) {
	if !c.on {
		return Verdict{}, false
	}
	b, err := os.ReadFile(filepath.Join(c.dir, k+".json"))
	if err != nil {
		return Verdict{}, false
	}
	var v Verdict
	if json.Unmarshal(b, &v) != nil {
		return Verdict{}, false
	}
	return v, true
}

func (c *cache) put(k string, v Verdict) {
	if !c.on {
		return
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return
	}
	b, _ := json.Marshal(v)
	os.WriteFile(filepath.Join(c.dir, k+".json"), b, 0o644)
}

// runWithRetry calls the model for the batch with up to three attempts,
// keeping any partial verdicts returned before a transient failure.
func runWithRetry(ctx context.Context, c *client, j job) (map[string]Verdict, error) {
	merged := make(map[string]Verdict, len(j.rules))
	pending := j.rules
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			wait := time.Duration(attempt*attempt) * 2 * time.Second
			if _, ok := err.(err429); ok {
				wait = time.Duration(attempt) * 6 * time.Second
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return merged, ctx.Err()
			}
		}
		var v map[string]Verdict
		v, err = c.call(ctx, job{unit: j.unit, rules: pending})
		maps.Copy(merged, v)
		if err == nil || errors.Is(err, errBudgetExhausted) || ctx.Err() != nil {
			break
		}
		pending = unansweredRules(pending, merged)
		if len(pending) == 0 {
			err = nil
			break
		}
	}
	return merged, err
}

func unansweredRules(rules []Rule, done map[string]Verdict) []Rule {
	var rest []Rule
	for _, r := range rules {
		if _, ok := done[r.ID]; !ok {
			rest = append(rest, r)
		}
	}
	return rest
}
