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
	"sync/atomic"
	"testing"
	"time"
)

func TestFollowUpDoesNotRequeryAnsweredRules(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var content string
		if n == 1 {
			// Omit todo-no-owner on first call; answer comment-long.
			content = `{"comment-long":{"lint":false}}`
		} else {
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

func TestDefaultIncludeHasNonGoLanguages(t *testing.T) {
	inc := defaultInclude()
	for _, want := range []string{"*.go", "*.md", "*.ts", "*.py", "*.rs"} {
		if !slices.Contains(inc, want) {
			t.Errorf("defaultInclude missing %q: %v", want, inc)
		}
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
	if severityRank("low") != 0 || severityRank("med") != 1 || severityRank("high") != 2 {
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
	cfg := "severity: med\nrules:\n  - off: md-jargon\nexclude:\n  - '**/fixtures/**'\n"
	if err := os.WriteFile(filepath.Join(dir, "lintdec.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	pc, warns := loadProjectConfig(dir)
	if len(warns) != 0 || pc.Severity != "med" || !slices.Contains(pc.RulesOff, "md-jargon") || !slices.Contains(pc.Exclude, "**/fixtures/**") {
		t.Errorf("unexpected project config: %+v warns=%v", pc, warns)
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
			if !strings.Contains(body.State, "TODO: fix") || len(body.Questions) != 2 {
				t.Errorf("unexpected systemone payload: %+v", body)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"model": "strands-decider-12B-gemma4-v1-2610",
				"answers": map[string]any{
					"comment-long":  map[string]any{"type": "noul", "noul": 0.12},
					"todo-no-owner": map[string]any{"type": "noul", "noul": 0.93},
				},
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

	r1, _ := findRule("comment-long")
	r2, _ := findRule("todo-no-owner")
	cl := &client{base: srv.URL, protocol: "systemone", model: models[0], threshold: 0.75, httpc: srv.Client()}
	j := job{unit: Unit{Kind: "Comment", File: "a.go", Start: 1, End: 1, Text: "// TODO: fix"}, rules: []Rule{r1, r2}}
	got, err := cl.call(context.Background(), j)
	if err != nil {
		t.Fatalf("systemone call error: %v", err)
	}
	if got["comment-long"].Lint {
		t.Errorf("comment-long should be clean at 0.12: %+v", got["comment-long"])
	}
	v := got["todo-no-owner"]
	if !v.Lint || v.Severity != "high" || v.Confidence != 0.93 || v.Reason != r2.Summary {
		t.Errorf("unexpected todo-no-owner verdict: %+v", v)
	}
}
