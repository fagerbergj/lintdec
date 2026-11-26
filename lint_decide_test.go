package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheKeysIncludeProviderEndpoint(t *testing.T) {
	unit := Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// x"}
	cache := newCache(true)
	a := &client{base: "https://first.example/v1", model: "same"}
	b := &client{base: "https://second.example/v1", model: "same"}
	if cache.key(a.cacheModel(), "0", "comment-long", a.cacheRuleVersion("comment-long"), unit) == cache.key(b.cacheModel(), "0", "comment-long", b.cacheRuleVersion("comment-long"), unit) {
		t.Fatal("different providers must not share cached verdicts")
	}
}

func TestFollowUpDoesNotRequeryAnsweredRules(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		n := calls.Add(1)
		var content string
		if n == 1 {
			// Omit todo-no-owner on first call; answer comment-long.
			content = `{"comment-long":{"lint":false}}`
		} else {
			user := ""
			if len(req.Messages) > 0 {
				user = req.Messages[len(req.Messages)-1].Content
			}
			if user == "" || strings.Contains(user, "comment-long") || !strings.Contains(user, "todo-no-owner") {
				t.Errorf("follow-up must ask only for missing rule, got prompt:\n%s", user)
			}
			// Obey follow-up: reply with ONLY the missing rule.
			content = `{"todo-no-owner":{"lint":true,"severity":"med","confidence":0.9,"line":1,"reason":"no owner"}}`
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
	defer srv.Close()

	r1, _ := findRule("comment-long")
	r2, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, model: "m", httpc: srv.Client()}
	j := job{
		unit:  Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1, Text: "// TODO: fix"},
		rules: []Rule{r1, r2},
	}
	got, err := cl.call(context.Background(), j)
	if err != nil {
		t.Fatalf("call error: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("want exactly 2 model calls (initial + follow-up), got %d", calls.Load())
	}
	if got["comment-long"].Lint || !got["todo-no-owner"].Lint {
		t.Errorf("unexpected verdicts: %+v", got)
	}
}

func TestBudgetRejectsNegativeLimit(t *testing.T) {
	if _, err := parseLintFlags([]string{"--budget=-1"}); err == nil {
		t.Fatal("negative budget must not disable the limit")
	}
}

func TestBudgetIncompleteRunExitsTwo(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, findings := range [][]Finding{nil, {{Rule: "comment-long", Severity: "error", File: "a.go", Line: 1, Message: "issue"}}} {
		var code int
		out := captureStdout(t, func() {
			code = emitReport(poolResult{findings: findings, budgetSkipped: 1}, "info", "json", 2, newProgress(2, true), true)
		})
		if code != 2 || (len(findings) > 0 && !strings.Contains(out, "comment-long")) {
			t.Fatalf("incomplete run: code=%d output=%q", code, out)
		}
	}
}

func TestBudgetCapsFollowupInferenceRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"comment-long":{"lint":false}}`}}}})
	}))
	defer srv.Close()
	r1, _ := findRule("comment-long")
	r2, _ := findRule("todo-no-owner")
	j := job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{r1, r2}}
	cl := &client{base: srv.URL, model: "m", httpc: srv.Client()}
	result := runPool(context.Background(), []job{j}, lintOpts{workers: 1, budget: 1}, cl, newCache(false), newProgress(1, true), nil, nil)
	if calls.Load() != 1 || result.budgetSkipped != 1 {
		t.Fatalf("budget=1 made %d inference requests, skipped=%d", calls.Load(), result.budgetSkipped)
	}
}

func TestBudgetCapsConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"comment-long":{"lint":false}}`}}}})
	}))
	defer srv.Close()
	rule, _ := findRule("comment-long")
	jobs := make([]job, 20)
	for i := range jobs {
		jobs[i] = job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// x"}, rules: []Rule{rule}}
	}
	cl := &client{base: srv.URL, model: "m", httpc: srv.Client()}
	result := runPool(context.Background(), jobs, lintOpts{workers: 8, budget: 3}, cl, newCache(false), newProgress(len(jobs), true), nil, nil)
	if calls.Load() != 3 || result.budgetSkipped != 17 {
		t.Fatalf("calls=%d skipped=%d", calls.Load(), result.budgetSkipped)
	}
}

func TestRunWithRetryHonorsCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	r1, _ := findRule("comment-long")
	cl := &client{base: srv.URL, model: "m", httpc: srv.Client()}
	j := job{unit: Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1, Text: "// x"}, rules: []Rule{r1}}

	start := time.Now()
	_, err := runWithRetry(ctx, cl, j)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation waited too long: %v", time.Since(start))
	}
}

func TestClientRejectsMalformedURL(t *testing.T) {
	r1, _ := findRule("comment-long")
	cl := &client{base: "http://bad host/v1", model: "m", httpc: &http.Client{}}
	j := job{unit: Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1, Text: "// x"}, rules: []Rule{r1}}
	if _, err := cl.call(context.Background(), j); err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

func TestCollectFilesAllowsExternalPaths(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "app.ts")
	if err := os.WriteFile(f, []byte("export const x = () => 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := collectFiles([]string{dir}, defaultInclude(), nil)
	if err != nil {
		t.Fatalf("collectFiles(%s): %v", dir, err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "app.ts") {
		t.Errorf("got %v, want [app.ts]", got)
	}
}

func TestSeverityRankRejectsUnknown(t *testing.T) {
	if severityRank("info") != 0 || severityRank("warning") != 1 || severityRank("error") != 2 || severityRank("high") >= 0 {
		t.Fatal("known severities ranked wrong")
	}
	if severityRank("medium") >= 0 || severityRank("") >= 0 {
		t.Fatal("unknown severities must return < 0")
	}
}

func TestDumpUnitsWorksWithoutProvider(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LINTDEC_PROVIDER", "")
	t.Setenv("LINTDEC_BASE_URL", "")
	dir := t.TempDir()
	f := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(f, []byte("package p\nfunc f() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := lintCmd([]string{"--dump-units", f}); code != 0 {
		t.Errorf("--dump-units exited %d without provider, want 0", code)
	}
}

func TestIsIgnoredAndLoadProjectConfig(t *testing.T) {
	lines := map[string][]string{
		"a.go": {"//lintdec:ignore comment-long", "func f() {}"},
	}
	j := job{unit: Unit{File: "a.go", Start: 2, End: 2}}
	if !isIgnored("comment-long", j, 2, lines) {
		t.Error("expected comment-long to be ignored")
	}
	if isIgnored("flakey-test", j, 2, lines) {
		t.Error("flakey-test should not be ignored")
	}

	dir := t.TempDir()
	cfg := "severity: warning\nrules:\n  - off: md-jargon\nexclude:\n  - '**/fixtures/**'\n"
	if err := os.WriteFile(filepath.Join(dir, "lintdec.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	pc, warns := loadProjectConfig(dir)
	if len(warns) != 0 || pc.Severity != "warning" || !slices.Contains(pc.RulesOff, "md-jargon") || !slices.Contains(pc.Exclude, "**/fixtures/**") {
		t.Errorf("unexpected project config: %+v warns=%v", pc, warns)
	}
}

func TestSystemOneRejectsMissingAndInvalidProbabilities(t *testing.T) {
	for _, value := range []string{`{}`, `{"noul":null}`, `{"noul":-0.1}`, `{"noul":1.1}`} {
		t.Run(value, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"answers":{"todo-no-owner":` + value + `}}`))
			}))
			defer srv.Close()
			rule, _ := findRule("todo-no-owner")
			cl := &client{base: srv.URL, protocol: "systemone", model: "jev", httpc: srv.Client()}
			_, err := cl.call(context.Background(), job{unit: Unit{File: "a.go", Kind: "Comment", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{rule}})
			if err == nil {
				t.Fatalf("accepted invalid answer %s", value)
			}
		})
	}
}

func TestSystemOneCallAndHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "model": "strands-decider-12B-gemma4-v1-2610"})
		case "/v1/systemone":
			var body struct {
				State     string                    `json:"state"`
				Questions map[string]map[string]any `json:"questions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("bad request body: %v", err)
			}
			if !strings.Contains(body.State, "TODO: fix") || len(body.Questions) != 1 {
				t.Errorf("want 1 rule per systemone call, got: %+v", body)
			}
			scores := map[string]float64{"comment-tautology": 0.12, "todo-no-owner": 0.93}
			answers := map[string]any{}
			for id := range body.Questions {
				answers[id] = map[string]any{"type": "noul", "noul": scores[id]}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"model":   "strands-decider-12B-gemma4-v1-2610",
				"answers": answers,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	models, err := fetchModels(Provider{BaseURL: srv.URL + "/v1", Protocol: "systemone"})
	if err != nil || !slices.Equal(models, []string{"strands-decider-12B-gemma4-v1-2610"}) {
		t.Fatalf("fetchModels systemone: %v %v", models, err)
	}

	r1, _ := findRule("comment-tautology")
	r2, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, protocol: "systemone", model: models[0], threshold: 0.75, httpc: srv.Client()}
	j := job{unit: Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1, Text: "// TODO: fix"}, rules: []Rule{r1, r2}}
	got, err := cl.call(context.Background(), j)
	if err != nil {
		t.Fatalf("systemone call error: %v", err)
	}
	if got["comment-tautology"].Lint {
		t.Errorf("comment-tautology should be clean at 0.12: %+v", got["comment-tautology"])
	}
	v := got["todo-no-owner"]
	if !v.Lint || v.Severity != "high" || v.Confidence != 0.93 || v.Reason != r2.Summary {
		t.Errorf("unexpected todo-no-owner verdict: %+v", v)
	}
}

func TestSystemOneCacheReusesAcrossThresholds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"model": "clef",
			"answers": map[string]any{
				"todo-no-owner": map[string]any{"type": "noul", "noul": 0.80},
			},
		})
	}))
	defer srv.Close()

	r, _ := findRule("todo-no-owner")
	u := Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1, Text: "// TODO: fix"}
	jobs := []job{{unit: u, rules: []Rule{r}}}
	ch := &cache{dir: t.TempDir(), on: true}
	lines := map[string][]string{"a.go": {"// TODO: fix"}}

	cl75 := &client{base: srv.URL, protocol: "systemone", model: "clef", threshold: 0.75, calibrate: true, httpc: srv.Client()}
	res75 := runPool(context.Background(), jobs, lintOpts{workers: 1, quiet: true}, cl75, ch, newProgress(1, true), lines, nil)
	if len(res75.findings) != 1 || calls.Load() != 1 {
		t.Fatalf("at 0.75: findings=%d calls=%d, want 1/1", len(res75.findings), calls.Load())
	}

	cl85 := &client{base: srv.URL, protocol: "systemone", model: "clef", threshold: 0.85, calibrate: true, httpc: srv.Client()}
	res85 := runPool(context.Background(), jobs, lintOpts{workers: 1, quiet: true}, cl85, ch, newProgress(1, true), lines, nil)
	if len(res85.findings) != 0 || calls.Load() != 1 {
		t.Fatalf("at 0.85: findings=%d calls=%d, want 0/1 (must hit cache)", len(res85.findings), calls.Load())
	}
}

func TestParseVerdictsSkipsEarlierProseBraces(t *testing.T) {
	r, _ := findRule("comment-long")
	j := job{unit: Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1}, rules: []Rule{r}}
	raw := "Checking {example} in prose before JSON:\n{\"comment-long\":{\"lint\":false}}"
	got, missing, err := parseVerdicts(raw, j)
	if err != nil || len(missing) != 0 || got["comment-long"].Lint {
		t.Fatalf("parseVerdicts failed with prose braces: got=%+v missing=%v err=%v", got, missing, err)
	}
}

func TestDecodeCompletionFallsBackToReasoning(t *testing.T) {
	body := `{"choices":[{"message":{"content":"  \n","reasoning":"{\"comment-long\":{\"lint\":false}}"}}]}`
	var usage *chatUsage
	got, err := decodeCompletion(strings.NewReader(body), &usage)
	if err != nil || !strings.Contains(got, "comment-long") {
		t.Fatalf("reasoning fallback failed: got=%q err=%v", got, err)
	}
}

func TestParseVerdictsUnbalancedFallback(t *testing.T) {
	r, _ := findRule("comment-long")
	j := job{unit: Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1}, rules: []Rule{r}}
	if _, _, err := parseVerdicts(`{"comment-long": invalid}`, j); err == nil || !strings.Contains(err.Error(), "bad model JSON") {
		t.Fatalf("expected bad model JSON error, got %v", err)
	}
	if _, _, err := parseVerdicts(`{"comment-long":{"lint":false},}`, j); err == nil || !strings.Contains(err.Error(), "bad model JSON") {
		t.Fatalf("malformed outer JSON must not fall back to inner object, got %v", err)
	}
	if _, _, err := parseVerdicts(`{"unrelated":"value"}`, j); err == nil || !strings.Contains(err.Error(), "no expected rule keys") {
		t.Fatalf("unrelated JSON object must error, got %v", err)
	}
	if _, _, err := parseVerdicts("no braces here", j); err == nil || !strings.Contains(err.Error(), "no JSON") {
		t.Fatalf("expected no JSON error, got %v", err)
	}
}

func TestSystemOnePartialRetryKeepsSucceededVerdicts(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var failOnce atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]map[string]any `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for id := range body.Questions {
			mu.Lock()
			seen = append(seen, id)
			mu.Unlock()
			if id == "todo-no-owner" && !failOnce.Swap(true) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"answers": map[string]any{id: map[string]any{"type": "noul", "noul": 0.9}},
			})
		}
	}))
	defer srv.Close()

	r1, _ := findRule("comment-tautology")
	r2, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, protocol: "systemone", model: "clef", threshold: 0.75, httpc: srv.Client()}
	j := job{unit: Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1, Text: "// TODO"}, rules: []Rule{r1, r2}}
	got, err := runWithRetry(context.Background(), cl, j)
	if err != nil || len(got) != 2 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{"comment-tautology", "todo-no-owner", "todo-no-owner"}) {
		t.Errorf("succeeded rule was re-requested on retry: %v", seen)
	}
}

func TestRunPoolReportsPartialVerdictsWhenSiblingErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]map[string]any `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body.Questions["comment-long"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{"todo-no-owner": map[string]any{"type": "noul", "noul": 0.95}},
		})
	}))
	defer srv.Close()

	r1, _ := findRule("todo-no-owner")
	r2, _ := findRule("comment-long")
	u := Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1, Text: "// TODO: fix"}
	cl := &client{base: srv.URL, protocol: "systemone", model: "clef", threshold: 0.75, httpc: srv.Client()}
	res := runPool(context.Background(), []job{{unit: u, rules: []Rule{r1, r2}}}, lintOpts{workers: 1, quiet: true}, cl, &cache{dir: t.TempDir(), on: true}, newProgress(1, true), map[string][]string{"a.go": {u.Text}}, nil)
	if res.errs != 1 || len(res.erroredKeys) != 1 || len(res.findings) != 1 || res.findings[0].Rule != "todo-no-owner" {
		t.Fatalf("want 1 finding + 1 error for sibling failure: errs=%d errored=%v findings=%+v", res.errs, res.erroredKeys, res.findings)
	}
}

func TestCacheKeyUsesBatchVersionForChatAndPerRuleForSystemOne(t *testing.T) {
	chat := &client{base: "http://x/v1", model: "m"}
	s1 := &client{base: "http://x/v1", protocol: "systemone", model: "m"}
	if chat.cacheRuleVersion("comment-long") != allRulesVersion {
		t.Errorf("chat should use catalogue version, got %q", chat.cacheRuleVersion("comment-long"))
	}
	if s1.cacheRuleVersion("comment-long") != ruleVersion("comment-long") {
		t.Errorf("systemone should use per-rule version, got %q", s1.cacheRuleVersion("comment-long"))
	}
	if s1.cacheRuleVersion("comment-long") == s1.cacheRuleVersion("todo-no-owner") {
		t.Error("distinct rules should have distinct per-rule versions")
	}
}

func TestParseVerdictsIgnoresTrailingProseBraces(t *testing.T) {
	r, _ := findRule("comment-long")
	j := job{unit: Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1}, rules: []Rule{r}}
	cases := []struct {
		name string
		raw  string
		lint bool
	}{
		{"trailing non-json brace", `{"comment-long":{"lint":false}}` + "\nNote: {kind: 'pass'} is valid.", false},
		{"trailing valid non-verdict json", `{"comment-long":{"lint":true,"severity":"med","confidence":0.9,"reason":"too long"}}` + "\nExample: {\"kind\":\"pass\"}", true},
		{"earlier unclosed brace in prose", `Checking {unclosed prose` + "\n" + `{"comment-long":{"lint":false}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, missing, err := parseVerdicts(tc.raw, j)
			if err != nil || len(missing) != 0 || got["comment-long"].Lint != tc.lint {
				t.Fatalf("got=%+v missing=%v err=%v", got, missing, err)
			}
		})
	}
}

func TestExplicitThresholdOverridesRuleFloorWhileKeepingVeto(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"model": "clef",
			"answers": map[string]any{
				"md-prose-list":      map[string]any{"type": "noul", "noul": 0.80},
				"md-prose-list~veto": map[string]any{"type": "noul", "noul": 0.30},
			},
		})
	}))
	defer srv.Close()

	rule, _ := findRule("md-prose-list")
	u := Unit{Kind: "Paragraph", File: "a.md", Start: 1, End: 1, Text: "a, b, c, d, e"}
	jobs := []job{{unit: u, rules: []Rule{rule}}}
	ch := &cache{dir: t.TempDir(), on: true}
	lines := map[string][]string{"a.md": {u.Text}}

	// Default run uses ruleFloor("md-prose-list") = 0.85, so 0.80 is suppressed.
	clDefault := newLintClient(endpoint{BaseURL: srv.URL, Protocol: "systemone", Model: "clef"}, lintOpts{threshold: 0.5})
	clDefault.httpc = srv.Client()
	resDefault := runPool(context.Background(), jobs, lintOpts{workers: 1, quiet: true}, clDefault, ch, newProgress(1, true), lines, nil)
	if len(resDefault.findings) != 0 {
		t.Fatalf("default floor should suppress 0.80 (< 0.85), got %d findings", len(resDefault.findings))
	}

	// Explicit --threshold 0.75 lowers the floor to 0.75 while keeping vetoes active.
	clExplicit := newLintClient(endpoint{BaseURL: srv.URL, Protocol: "systemone", Model: "clef"}, lintOpts{
		threshold: 0.75,
		setFlags:  map[string]bool{"threshold": true},
	})
	clExplicit.httpc = srv.Client()
	resExplicit := runPool(context.Background(), jobs, lintOpts{workers: 1, quiet: true}, clExplicit, ch, newProgress(1, true), lines, nil)
	if len(resExplicit.findings) != 1 || calls.Load() != 1 {
		t.Fatalf("explicit --threshold 0.75 should report 0.80 from cache: findings=%d calls=%d", len(resExplicit.findings), calls.Load())
	}

	// Out-of-range and NaN --threshold values are rejected.
	for _, bad := range []string{"--threshold=0", "--threshold=-0.1", "--threshold=1.1", "--threshold=NaN"} {
		if _, err := parseLintFlags([]string{bad}); err == nil {
			t.Errorf("parseLintFlags(%q) should fail", bad)
		}
	}
}
