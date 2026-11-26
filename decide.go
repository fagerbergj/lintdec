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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type client struct {
	base      string
	protocol  string // ""|"openai" or "systemone"
	model     string
	apiKey    string
	temp      float64
	threshold float64
	httpc     *http.Client
}

type Verdict struct {
	Lint       bool    `json:"lint"`
	Severity   string  `json:"severity"`
	Confidence float64 `json:"confidence"`
	Line       *int    `json:"line"`
	Reason     string  `json:"reason"`
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

// callSystemOne evaluates one rule per /v1/systemone call against the same
// state so long negative clauses in sibling rubrics do not suppress each other's
// noul probabilities and per-rule cache keys stay independent of --rules.
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
			return nil, err
		}
		out[r.ID] = v
	}
	return out, nil
}

func (c *client) callSystemOneRule(ctx context.Context, state string, r Rule) (Verdict, error) {
	questions := map[string]map[string]string{
		r.ID: {
			"type":         "noul",
			"instructions": fmt.Sprintf("Does the TARGET violate this rule? %s — %s. CRITERIA: %s", r.ID, r.Summary, r.Rubric),
		},
	}
	payload, _ := json.Marshal(map[string]any{
		"model":     c.model,
		"state":     state,
		"questions": questions,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", systemOneRoot(c.base)+"/v1/systemone", bytes.NewReader(payload))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	res, err := c.httpc.Do(req)
	if err != nil {
		return Verdict{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return Verdict{}, err429{strings.TrimSpace(string(b))}
	}
	if res.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 400))
		return Verdict{}, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var resp struct {
		Answers map[string]struct {
			Noul float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return Verdict{}, err
	}
	ans, ok := resp.Answers[r.ID]
	if !ok {
		return Verdict{}, fmt.Errorf("systemone response missing answer for %s", r.ID)
	}
	return applySystemOneThreshold(Verdict{
		Confidence: ans.Noul,
		Reason:     r.Summary,
	}, c.threshold), nil
}

// applySystemOneThreshold derives Lint and Severity from a cached or fresh noul
// probability so --threshold sweeps reuse the cache without re-running the model.
func applySystemOneThreshold(v Verdict, threshold float64) Verdict {
	if threshold <= 0 {
		threshold = 0.75
	}
	v.Lint = v.Confidence >= threshold
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

func (c *client) callOnce(ctx context.Context, j job, followUp string) (map[string]Verdict, []string, error) {
	var rubric strings.Builder
	for i, r := range j.rules {
		fmt.Fprintf(&rubric, "%d. %s — %s\n   CRITERIA: %s\n", i+1, r.ID, r.Summary, r.Rubric)
	}
	var sb strings.Builder
	sb.WriteString("RULES:\n")
	sb.WriteString(rubric.String())
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
	payload, _ := json.Marshal(map[string]any{
		"model": c.model, "temperature": c.temp, "max_tokens": 4096,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": sb.String()},
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.base, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	res, err := c.httpc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return nil, nil, err429{strings.TrimSpace(string(b))}
	}
	if res.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 400))
		return nil, nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				Reasoning *string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, nil, err
	}
	if len(out.Choices) == 0 {
		return nil, nil, errors.New("empty completion")
	}
	raw := ""
	if out.Choices[0].Message.Content != nil && strings.TrimSpace(*out.Choices[0].Message.Content) != "" {
		raw = *out.Choices[0].Message.Content
	} else if out.Choices[0].Message.Reasoning != nil {
		raw = *out.Choices[0].Message.Reasoning
	}
	return parseVerdicts(raw, j)
}

// extractJSONObject finds the last balanced {...} JSON object in raw so prose or
// reasoning text mentioning braces earlier in the reply does not break parsing.
func extractJSONObject(raw string) (string, bool) {
	end := strings.LastIndex(raw, "}")
	if end < 0 {
		return "", false
	}
	inStr, esc, depth := false, false, 0
	lastOpen := -1
	for i := 0; i <= end; i++ {
		ch := raw[i]
		if esc {
			esc = false
			continue
		}
		if inStr {
			if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				lastOpen = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && i == end && lastOpen >= 0 {
					return raw[lastOpen : end+1], true
				}
			}
		}
	}
	s := strings.Index(raw, "{")
	if s < 0 || end <= s {
		return "", false
	}
	return raw[s : end+1], true
}

// parseVerdicts extracts the rule-id → verdict map. It returns the verdicts
// parsed so far plus the ids the model omitted; malformed JSON is an error.
func parseVerdicts(raw string, j job) (map[string]Verdict, []string, error) {
	blob, ok := extractJSONObject(raw)
	if !ok {
		return nil, nil, fmt.Errorf("no JSON in model reply: %.80q", raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		return nil, nil, fmt.Errorf("bad model JSON: %w", err)
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
		if v.Line != nil && (*v.Line < j.unit.Start || *v.Line > j.unit.End) {
			l := j.unit.Start
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
		out[r.ID] = v
	}
	return out, missing, nil
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

func (c *cache) key(model, tempStr string, r Rule, u Unit) string {
	h := sha256.New()
	abs, _ := filepath.Abs(u.File)
	// ruleVersion folds the system prompt and this rule's rubric into the key, so
	// editing one rubric invalidates only that rule's verdicts; the absolute path
	// makes the cache stable across working directories.
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s\n%s\n%s", model, tempStr, r.ID, u.Kind, ruleVersion(r), abs, u.Text, u.Context)
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

// runWithRetry calls the model for the batch with up to three attempts.
// 429s (gateway concurrency limit) get a long backoff; ctx cancels immediately.
func runWithRetry(ctx context.Context, c *client, j job) (map[string]Verdict, error) {
	var v map[string]Verdict
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
				return nil, ctx.Err()
			}
		}
		v, err = c.call(ctx, j)
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	return v, err
}
