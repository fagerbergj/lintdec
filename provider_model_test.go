package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

// A provider whose /models answered must end with a saved and active model,
// otherwise lint has nothing to call.
func TestProviderAddPersistsPickedModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "m-1"}, {"id": "m-2"}},
		})
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	orig := stdinReader
	defer func() { stdinReader = orig }()
	// name, protocol, base URL, api key, test?, set active?, pick #2
	stdinReader = bufio.NewReader(strings.NewReader("clef\n\n" + srv.URL + "\n\ny\ny\n2\n"))

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if code := providerAdd(&cfg, nil); code != 0 {
		t.Fatalf("providerAdd exit %d", code)
	}

	b, err := os.ReadFile(filepath.Join(home, "lintdec", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Active != "clef" || got.ActiveModel != "m-2" {
		t.Errorf("active = %s/%q, want clef/m-2", got.Active, got.ActiveModel)
	}
	if !slices.Contains(got.Providers["clef"].Models, "m-2") {
		t.Errorf("saved models = %v, want m-2 in it", got.Providers["clef"].Models)
	}
}

func TestModelListShowsModellessProvider(t *testing.T) {
	c := Config{Providers: map[string]Provider{
		"clef": {BaseURL: "http://127.0.0.1:11436/v1"},
	}}
	out := captureStdout(t, func() { modelList(c) })
	if !strings.Contains(out, "clef") || !strings.Contains(out, "no models yet") {
		t.Errorf("model list hid the provider: %q", out)
	}
}

func TestSystemOneHealthNonJSONHintsOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "OK")
	}))
	defer srv.Close()

	_, err := fetchSystemOneModel(Provider{Protocol: "systemone", BaseURL: srv.URL + "/v1"})
	if err == nil || !strings.Contains(err.Error(), "protocol openai") {
		t.Fatalf("err = %v, want a hint to use protocol openai", err)
	}
}
