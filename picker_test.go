package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestProviderPickerSelectsAndKeepsActive(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := stdinReader
	defer func() { stdinReader = old }()
	cfg := Config{Active: "local", ActiveModel: "second", Providers: map[string]Provider{
		"local": {BaseURL: "http://localhost:11434/v1", Models: []string{"first", "second"}},
		"work":  {BaseURL: "https://example.test/v1", Models: []string{"work-model"}},
	}}
	stdinReader = bufio.NewReader(strings.NewReader("\n"))
	if code := providerPick(&cfg); code != 0 || cfg.ActiveModel != "second" {
		t.Fatalf("enter should retain the active model: code=%d model=%q", code, cfg.ActiveModel)
	}
	stdinReader = bufio.NewReader(strings.NewReader("2\n"))
	if code := providerPick(&cfg); code != 0 || cfg.Active != "work" || cfg.ActiveModel != "work-model" {
		t.Fatalf("provider selection: code=%d active=%s model=%s", code, cfg.Active, cfg.ActiveModel)
	}
}

func TestModelPickerFormFilteringAndProviderIdentity(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	choices := []modelChoice{{"alpha", "shared"}, {"beta", "shared"}, {"beta", "other"}}
	selected := choices[1]
	form := modelPickerForm(choices, &selected).WithWidth(80)
	form.Init()
	if selected != choices[1] {
		t.Fatalf("active choice not preselected: %+v", selected)
	}
	view := ansi.Strip(form.View())
	for _, want := range []string{"Choose a model", "/ to filter", "alpha", "beta", "shared"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in %q", want, view)
		}
	}
	form.Update(tea.KeyPressMsg(tea.Key{Code: '/'}))
	for _, r := range "alpha" {
		form.Update(tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
	}
	view = ansi.Strip(form.View())
	if !strings.Contains(view, "alpha") || strings.Contains(view, "beta  shared") {
		t.Fatalf("filter did not narrow provider choices: %q", view)
	}
	form.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if selected != choices[0] {
		t.Fatalf("selected wrong provider for duplicate model: %+v", selected)
	}
}

func TestModelPickerFormKeyboardSelection(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	choices := []modelChoice{{"alpha", "shared"}, {"beta", "shared"}}
	selected := choices[0]
	form := modelPickerForm(choices, &selected).WithWidth(80)
	form.Init()
	form.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if selected != choices[1] {
		t.Fatalf("arrow selected wrong provider: %+v", selected)
	}
	form.Update(tea.KeyPressMsg(tea.Key{Code: '/'}))
	for _, r := range "missing" {
		form.Update(tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
	}
	form.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if form.State == huh.StateCompleted {
		t.Fatal("empty filtered list submitted")
	}
}

func TestModelPickerFormBoundsLargeCatalog(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	choices := make([]modelChoice, 100)
	for i := range choices {
		choices[i] = modelChoice{"provider", fmt.Sprintf("model-%02d", i)}
	}
	selected := choices[0]
	form := modelPickerForm(choices, &selected).WithWidth(80)
	form.Init()
	form.Update(tea.WindowSizeMsg{Width: 80, Height: 15})
	view := ansi.Strip(form.View())
	if lines := strings.Count(view, "\n") + 1; lines > 15 {
		t.Fatalf("picker exceeds terminal height: %d lines", lines)
	}
}

func TestModelPickerFormActiveChoiceVisible(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	choices := make([]modelChoice, 30)
	for i := range choices {
		choices[i] = modelChoice{"provider", fmt.Sprintf("model-%02d", i)}
	}
	selected := choices[29]
	form := modelPickerForm(choices, &selected)
	form.Init()
	form.Update(tea.WindowSizeMsg{Width: 80, Height: 15})
	if view := ansi.Strip(form.View()); !strings.Contains(view, "model-29") || selected != choices[29] {
		t.Fatalf("active model not visible and selected: %+v, %s", selected, view)
	}
}

func TestModelPickerCancel(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	for _, input := range []string{"\x03", "\x1b"} {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			var out bytes.Buffer
			_, err := runModelPicker([]modelChoice{{"local", "model"}}, modelChoice{}, strings.NewReader(input), &out)
			if !errors.Is(err, huh.ErrUserAborted) {
				t.Fatalf("expected cancellation, got %v", err)
			}
		})
	}
}

func TestModelPickerFallbackAndLargeCatalog(t *testing.T) {
	var ids []string
	for i := range 25 {
		ids = append(ids, fmt.Sprintf("model-%02d", i))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[`)
		for i, id := range ids {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"id":%q}`, id)
		}
		fmt.Fprint(w, `]}`)
	}))
	defer srv.Close()
	p := Provider{BaseURL: srv.URL, APIKey: "private-secret"}
	old := stdinReader
	defer func() { stdinReader = old }()
	stdinReader = bufio.NewReader(strings.NewReader("unused\n"))
	out := captureStdout(t, func() {
		choices := fetchChoicesForProvider("local", p, true)
		if len(choices) != len(ids) || choices[24].model != ids[24] {
			t.Fatalf("interactive catalog truncated: %+v", choices)
		}
	})
	line, _ := stdinReader.ReadString('\n')
	if out != "" || line != "unused\n" {
		t.Fatalf("interactive discovery prompted: %q, remaining %q", out, line)
	}
	for _, mode := range []string{"redirected", "dumb", "accessible"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("LINTDEC_ACCESSIBLE", "0")
			if mode == "dumb" {
				t.Setenv("TERM", "dumb")
			}
			if mode == "accessible" {
				t.Setenv("LINTDEC_ACCESSIBLE", "1")
			}
			stdinReader = bufio.NewReader(strings.NewReader("MODEL-2\n2\n"))
			cfg := Config{Providers: map[string]Provider{"local": p}}
			out := captureStdout(t, func() {
				if code := modelPick(&cfg, []string{"local"}); code != 0 {
					t.Fatalf("fallback exited %d", code)
				}
			})
			if cfg.Active != "local" || cfg.ActiveModel != "model-21" || !slices.Contains(cfg.Providers["local"].Models, "model-21") {
				t.Fatalf("fallback selection: %+v", cfg)
			}
			if !strings.Contains(out, "25 models on local") || !strings.Contains(out, "pick number") || strings.Contains(out, "private-secret") {
				t.Fatalf("fallback output: %q", out)
			}
		})
	}
}

func TestModelPickerUsesSavedModelsWhenDiscoveryFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := Provider{BaseURL: srv.URL, Models: []string{"saved-one", "saved-two"}}
	choices := fetchChoicesForProvider("local", p, true)
	if !slices.Equal(choices, []modelChoice{{"local", "saved-one"}, {"local", "saved-two"}}) {
		t.Fatalf("saved choices: %+v", choices)
	}
}

func TestModelPickerEnterKeepsActive(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := stdinReader
	defer func() { stdinReader = old }()
	stdinReader = bufio.NewReader(strings.NewReader("\n"))
	cfg := Config{Active: "local", ActiveModel: "small", Providers: map[string]Provider{
		"local": {BaseURL: "http://localhost:11434/v1", Models: []string{"small", "large"}},
	}}
	if code := modelPick(&cfg, nil); code != 0 || cfg.ActiveModel != "small" {
		t.Fatalf("enter should retain the active model: code=%d model=%q", code, cfg.ActiveModel)
	}
}
