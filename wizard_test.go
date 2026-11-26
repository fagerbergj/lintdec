package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestProviderWizardPlainProviderFlows(t *testing.T) {
	t.Setenv("OPENROUTER_OIDC_TOKEN_FILE", "")
	t.Setenv("OPENROUTER_FEDERATION_POLICY_ID", "")
	tests := []struct {
		name, input, wantName, protocol, auth, base, key, issuer, client, resource, scopes, file, policy string
		forbidden                                                                                        []string
	}{
		{"openrouter OIDC", "1\n\n2\n2\n3\n\n\npublic-client\nno\nyes\n", "openrouter", "systemone", "oidc", "https://openrouter.ai/api/v1", "", "https://clerk.openrouter.ai", "public-client", "https://openrouter.ai/api/v1", "offline_access email inference", "", "", []string{"subject-token file", "OpenRouter API key (required", "resource / audience"}},
		{"openrouter workload", "1\nteam\n2\n1\n4\n\n/token\npolicy\nno\nyes\n", "team", "", "openrouter-oidc", "https://openrouter.ai/api/v1", "", "", "", "", "", "/token", "policy", []string{"OIDC public client ID", "OpenRouter API key (required"}},
		{"openrouter API key", "1\n\n2\n2\n1\n\nrouter-secret\nno\nyes\n", "openrouter", "systemone", "", "https://openrouter.ai/api/v1", "router-secret", "", "", "", "", "", "", []string{"OIDC public client ID", "subject-token file"}},
		{"openai", "2\n\n\nsecret\nno\nyes\n", "openai", "", "", "https://api.openai.com/v1", "secret", "", "", "", "", "", "", []string{"Protocol:", "OIDC public client ID"}},
		{"custom oidc", "3\n\n2\nhttps://api.example/v1\nhttps://issuer.example/\nclient\n\n\nno\nyes\n", "custom-oidc", "systemone", "oidc", "https://api.example/v1", "", "https://issuer.example/", "client", "https://api.example/v1", "offline_access", "", "", []string{"OpenRouter authentication", "subject-token file", "API key"}},
		{"local", "4\n\n\n\nsecret\nno\nyes\n", "local", "", "", "http://localhost:11434/v1", "secret", "", "", "", "", "", "", []string{"OpenRouter authentication", "OIDC public client ID"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			r, err := runProviderWizardPlain(newProviderWizardDraft(""), strings.NewReader(tt.input), &out, nil)
			if err != nil {
				t.Fatalf("%v; output %s", err, out.String())
			}
			if r.Name != tt.wantName || r.Provider.Protocol != tt.protocol || r.Provider.Auth != tt.auth || r.Provider.BaseURL != tt.base || r.Provider.APIKey != tt.key || r.OIDCIssuerURL != tt.issuer || r.OIDCClientID != tt.client || r.OIDCResource != tt.resource || strings.Join(r.OIDCScopes, " ") != tt.scopes || r.Provider.OIDCTokenFile != tt.file || r.Provider.FederationPolicyID != tt.policy {
				t.Fatalf("unexpected result: %+v", r)
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(out.String(), forbidden) {
					t.Errorf("irrelevant field %q in %q", forbidden, out.String())
				}
			}
			if tt.key != "" && strings.Contains(out.String(), tt.key) {
				t.Fatal("API key printed")
			}
			if r.Test || !r.Active {
				t.Fatalf("test/active: %+v", r)
			}
		})
	}
}

func TestProviderWizardOpenRouterQuickSetup(t *testing.T) {
	var out bytes.Buffer
	r, err := runProviderWizardPlain(newProviderWizardDraft("team"), strings.NewReader("1\n\n1\n"), &out, nil)
	if err != nil || r.Name != "team" || !r.BrowserOpenRouter || r.Provider.Protocol != "systemone" || r.Provider.BaseURL != "https://openrouter.ai/api/v1" || !r.Test || !r.Active {
		t.Fatalf("quick OpenRouter setup: %+v, err=%v, output=%s", r, err, out.String())
	}
	for _, field := range []string{"Model type:", "OpenRouter authentication:", "Base URL:", "OIDC public client ID:"} {
		if strings.Contains(out.String(), field) {
			t.Fatalf("quick setup asked for %s", field)
		}
	}
}

func TestProviderWizardQuickDiscardsAdvancedSettings(t *testing.T) {
	d := newProviderWizardDraft("team")
	d.kind, d.routerAdvanced, d.routerURL, d.routerAuth = "openrouter", false, "https://other.example/v1", "oidc"
	d.protocol, d.test, d.active = "openai", false, false
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
	r := d.result()
	if !r.BrowserOpenRouter || r.Provider.BaseURL != "https://openrouter.ai/api/v1" || r.Provider.Protocol != "systemone" || !r.Test || !r.Active {
		t.Fatalf("advanced settings leaked into quick setup: %+v", r)
	}
}

func TestProviderWizardHuhOpenRouterQuickShowsConsent(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	d := newProviderWizardDraft("")
	d.kind = "openrouter"
	f := providerWizardForm(d).WithWidth(80)
	f.Init()
	f.NextGroup()
	f.NextGroup()
	if view := ansi.Strip(f.View()); !strings.Contains(view, "OpenRouter setup") || !strings.Contains(view, "creating an API key") {
		t.Fatalf("quick setup did not explain sign-in: %s", view)
	}
	f.NextGroup()
	if f.State != huh.StateCompleted {
		t.Fatalf("quick setup unexpectedly requested another field: %s", ansi.Strip(f.View()))
	}
}

func TestProviderWizardHuhOpenRouterAdvancedAndBack(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	d := newProviderWizardDraft("")
	d.kind = "openrouter"
	f := providerWizardForm(d).WithWidth(80)
	f.Init()
	f.NextGroup()
	f.NextGroup()
	f.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if !d.routerAdvanced {
		t.Fatal("advanced setup was not selected")
	}
	f.NextGroup()
	if view := ansi.Strip(f.View()); !strings.Contains(view, "Model type") {
		t.Fatalf("advanced setup omitted model choice: %s", view)
	}
	wizardTestBack(t, f)
	if view := ansi.Strip(f.View()); !strings.Contains(view, "OpenRouter setup") {
		t.Fatalf("could not return to setup choice: %s", view)
	}
}

func TestProviderWizardOpenRouterBrowserSignIn(t *testing.T) {
	var out bytes.Buffer
	r, err := runProviderWizardPlain(newProviderWizardDraft(""), strings.NewReader("1\n\n2\n2\n2\n\nno\nyes\n"), &out, nil)
	if err != nil || !r.BrowserOpenRouter || r.Provider.APIKey != "" || r.Provider.Auth != "" || r.Provider.Protocol != "systemone" {
		t.Fatalf("browser sign-in proposal: %+v, err=%v", r, err)
	}
	if strings.Contains(out.String(), "OIDC public client ID") || strings.Contains(out.String(), "OpenRouter API key (required") {
		t.Fatalf("browser sign-in asked for an unnecessary credential: %s", out.String())
	}
}

func TestProviderWizardDefaultsAndOIDCOptions(t *testing.T) {
	d := newProviderWizardDraft("supplied")
	for _, tc := range []struct{ kind, protocol, base string }{
		{"openai", "systemone", "https://api.openai.com/v1"},
		{"openrouter", "systemone", "https://openrouter.ai/api/v1"},
		{"custom-oidc", "openai", ""},
		{"local", "openai", "http://localhost:11434/v1"},
		{"local", "systemone", "http://localhost:8000"},
	} {
		d.kind, d.protocol = tc.kind, tc.protocol
		if got := d.defaultEndpoint(); got != tc.base {
			t.Errorf("%s/%s: %q", tc.kind, tc.protocol, got)
		}
		if d.result().Name != "supplied" {
			t.Fatal("supplied name lost")
		}
	}

	d.kind, d.oidcURL, d.oidcIssuer, d.oidcClientID = "custom-oidc", "https://api.example/v1", "https://issuer.example/", "client"
	d.oidcResource, d.oidcScopes = "urn:custom:api", "openid profile custom"
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
	r := d.result()
	if r.OIDCIssuerURL != "https://issuer.example/" || r.OIDCResource != "urn:custom:api" || !slices.Equal(r.OIDCScopes, []string{"openid", "profile", "custom"}) {
		t.Fatalf("custom OIDC values: %+v", r)
	}
	d.oidcResource, d.oidcScopes = "", ""
	if r := d.result(); r.OIDCResource != "https://api.example/v1" || !slices.Equal(r.OIDCScopes, []string{"offline_access"}) {
		t.Fatalf("custom defaults: %+v", r)
	}
	d.kind, d.routerAuth, d.routerClientID, d.routerAdvanced = "openrouter", "oidc", "router-client", true
	if r := d.result(); r.OIDCIssuerURL != "https://clerk.openrouter.ai" || r.OIDCResource != "https://openrouter.ai/api/v1" || !slices.Equal(r.OIDCScopes, []string{"offline_access", "email", "inference"}) {
		t.Fatalf("OpenRouter defaults: %+v", r)
	}
}

func TestProviderWizardPlainCustomOIDCOverrides(t *testing.T) {
	var out bytes.Buffer
	input := "3\nmy-provider\n1\nhttps://api.example/v1\nhttps://issuer.example\nclient\nurn:resource\nopenid   custom\tprofile\nno\nyes\n"
	r, err := runProviderWizardPlain(newProviderWizardDraft(""), strings.NewReader(input), &out, nil)
	if err != nil || r.Name != "my-provider" || r.OIDCResource != "urn:resource" || !slices.Equal(r.OIDCScopes, []string{"openid", "custom", "profile"}) {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

func TestProviderWizardValidation(t *testing.T) {
	d := newProviderWizardDraft("")
	d.kind = "openai"
	if err := d.validate(); err == nil {
		t.Fatal("OpenAI API key must be required")
	}
	d.kind, d.oidcURL, d.oidcClientID = "custom-oidc", "http://example.test/v1", "client"
	if err := d.validate(); err == nil {
		t.Fatal("browser OIDC must not use an HTTP endpoint")
	}
	d.oidcURL = "https://example.test/v1"
	if err := d.validate(); err == nil {
		t.Fatal("custom OIDC needs issuer")
	}
	d.oidcIssuer, d.oidcClientID = "https://issuer.example", ""
	if err := d.validate(); err == nil {
		t.Fatal("custom OIDC needs public client ID")
	}
	d.kind, d.routerURL, d.routerClientID, d.routerAdvanced = "openrouter", "https://other.example/v1", "client", true
	if err := d.validate(); err == nil {
		t.Fatal("OpenRouter must reject other hosts")
	}
}

func wizardTestBack(t *testing.T, f *huh.Form) {
	t.Helper()
	_, cmd := f.GetFocusedField().Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab, Mod: tea.ModShift}))
	if cmd == nil {
		t.Fatal("shift-tab did not request back navigation")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		msg = batch[0]()
	}
	_, cmd = f.Update(msg)
	if cmd == nil {
		t.Fatal("back did not leave the field")
	}
	msg = cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		msg = batch[len(batch)-1]()
	}
	f.Update(msg)
}

func TestProviderWizardHuhFirstPickerAndBack(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	d := newProviderWizardDraft("")
	d.kind = "local"
	f := providerWizardForm(d).WithWidth(80)
	f.Init()
	view := ansi.Strip(f.View())
	for _, text := range []string{"Provider", "OpenRouter", "OpenAI", "Custom OIDC", "Local/custom endpoint"} {
		if !strings.Contains(view, text) {
			t.Fatalf("first screen missing %q: %q", text, view)
		}
	}
	if strings.Contains(view, "Model type") || strings.Contains(view, "OAuth") {
		t.Fatalf("unexpected first screen: %s", view)
	}
	f.NextGroup() // name
	f.NextGroup() // protocol
	f.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if d.protocol != "systemone" {
		t.Fatal("protocol arrow selection failed")
	}
	f.NextGroup() // local URL
	for _, c := range "bad-url" {
		f.Update(tea.KeyPressMsg(tea.Key{Code: c, Text: string(c)}))
	}
	if d.validate() == nil {
		t.Fatal("invalid endpoint accepted")
	}
	wizardTestBack(t, f)
	if !strings.Contains(ansi.Strip(f.View()), "Model type") {
		t.Fatal("could not back out of invalid URL")
	}
	f.NextGroup()
	if f.GetFocusedField().GetValue() != "bad-url" {
		t.Fatal("invalid input lost after back")
	}
	wizardTestBack(t, f) // protocol
	wizardTestBack(t, f) // name
	wizardTestBack(t, f) // provider
	if !strings.Contains(ansi.Strip(f.View()), "Local/custom endpoint") {
		t.Fatal("could not return to provider choice")
	}
	f.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	if d.kind != "custom-oidc" {
		t.Fatalf("provider arrow selection failed: %q", d.kind)
	}
}

func TestProviderWizardHuhOnlyShowsRelevantFields(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	for _, tc := range []struct {
		name, kind, auth string
		titles           []string
	}{
		{"openai", "openai", "", []string{"Provider", "Provider name", "Base URL", "OpenAI API key", "Test connection", "Set as active"}},
		{"router browser", "openrouter", "oidc", []string{"Provider", "Provider name", "OpenRouter setup", "Model type", "OpenRouter authentication", "Base URL", "OIDC issuer URL", "OIDC public client ID", "Test connection", "Set as active"}},
		{"router workload", "openrouter", "openrouter-oidc", []string{"Provider", "Provider name", "OpenRouter setup", "Model type", "OpenRouter authentication", "Base URL", "OIDC subject-token file", "federation policy ID", "Test connection", "Set as active"}},
		{"custom", "custom-oidc", "", []string{"Provider", "Provider name", "Model type", "Base URL", "OIDC issuer URL", "OIDC public client ID", "OIDC resource", "OIDC scopes", "Test connection", "Set as active"}},
		{"local", "local", "", []string{"Provider", "Provider name", "Model type", "Base URL", "API key", "Test connection", "Set as active"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newProviderWizardDraft("")
			d.kind = tc.kind
			if tc.auth != "" {
				d.routerAuth, d.routerAdvanced = tc.auth, true
			}
			f := providerWizardForm(d).WithWidth(80)
			f.Init()
			for i, title := range tc.titles {
				if i > 0 {
					f.NextGroup()
				}
				if view := ansi.Strip(f.View()); !strings.Contains(view, title) {
					t.Fatalf("step %d: wanted %q, got %q", i, title, view)
				}
			}
			f.NextGroup()
			if f.State != huh.StateCompleted {
				t.Fatalf("expected form to complete after %d steps, got extra step: %q", len(tc.titles), ansi.Strip(f.View()))
			}
		})
	}
}

func TestProviderWizardHuhBackFromEmptyFields(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("OPENROUTER_OIDC_TOKEN_FILE", "")
	t.Setenv("OPENROUTER_FEDERATION_POLICY_ID", "")
	for _, tc := range []struct {
		name, kind, auth, previous string
		moves                      int
	}{
		{"custom empty issuer", "custom-oidc", "", "Base URL", 4},
		{"custom empty client", "custom-oidc", "", "OIDC issuer URL", 5},
		{"workload empty file", "openrouter", "openrouter-oidc", "Base URL", 6},
		{"workload empty policy", "openrouter", "openrouter-oidc", "OIDC subject-token file", 7},
		{"OpenAI empty key", "openai", "", "Base URL", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newProviderWizardDraft("")
			d.kind = tc.kind
			if tc.auth != "" {
				d.routerAuth, d.routerAdvanced = tc.auth, true
			}
			f := providerWizardForm(d).WithWidth(80)
			f.Init()
			for i := 0; i < tc.moves; i++ {
				f.NextGroup()
			}
			if d.validate() == nil {
				t.Fatal("expected invalid draft")
			}
			wizardTestBack(t, f)
			if view := ansi.Strip(f.View()); !strings.Contains(view, tc.previous) {
				t.Fatalf("expected %q, got %q", tc.previous, view)
			}
		})
	}
}

func TestProviderWizardHuhMaskedAndRetry(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	d := newProviderWizardDraft("")
	d.kind = "openai"
	f := providerWizardForm(d).WithWidth(80)
	f.Init()
	for i := 0; i < 3; i++ {
		f.NextGroup()
	}
	for _, c := range "very-secret" {
		f.Update(tea.KeyPressMsg(tea.Key{Code: c, Text: string(c)}))
	}
	if d.openaiKey != "very-secret" || strings.Contains(f.View(), "very-secret") {
		t.Fatal("key missing or visible")
	}
	f.GetFocusedField().Blur()
	if strings.Contains(f.GetFocusedField().View(), "very-secret") {
		t.Fatal("key visible when blurred")
	}
	d.openaiURL = "bad-url"
	if d.validate() == nil {
		t.Fatal("invalid URL accepted")
	}
	d.issue = "Fix base URL"
	retry := providerWizardForm(d).WithWidth(80)
	retry.Init()
	if !strings.Contains(ansi.Strip(retry.View()), "Fix base URL") {
		t.Fatal("retry error not shown")
	}
	for i := 0; i < 3; i++ {
		retry.NextGroup()
	}
	if retry.GetFocusedField().GetValue() != "very-secret" || strings.Contains(retry.View(), "very-secret") {
		t.Fatal("retry did not retain masked key")
	}
}

func TestProviderWizardSwitchingProvidersDoesNotLeakValues(t *testing.T) {
	d := newProviderWizardDraft("")
	d.kind, d.openaiURL, d.openaiKey = "openai", "https://api.openai.com/v1", "secret"
	d.kind = "local"
	if r := d.result(); r.Provider.APIKey != "" || r.Provider.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("OpenAI values leaked to local: %+v", r)
	}
	d.localURL, d.localKey = "http://localhost:8080/v1", "local-secret"
	d.kind, d.routerAuth, d.routerClientID, d.routerAdvanced = "openrouter", "oidc", "router-client", true
	if r := d.result(); r.Provider.APIKey != "" || r.Provider.BaseURL != "https://openrouter.ai/api/v1" || r.OIDCClientID != "router-client" {
		t.Fatalf("local values leaked to OpenRouter: %+v", r)
	}
	d.kind = "custom-oidc"
	if r := d.result(); r.OIDCClientID != "" || r.Provider.BaseURL != "" {
		t.Fatalf("OpenRouter values leaked to custom: %+v", r)
	}
	d.kind = "openai"
	if r := d.result(); r.Provider.APIKey != "secret" || r.Provider.BaseURL != "https://api.openai.com/v1" {
		t.Fatal("going back lost original OpenAI values")
	}
}

func TestProviderWizardHuhCancel(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	f := providerWizardForm(newProviderWizardDraft(""))
	f.Init()
	f.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	if f.State != huh.StateAborted {
		t.Fatal("Ctrl-C did not cancel the form")
	}
}

func TestProviderWizardPlainBackCancelAndMasking(t *testing.T) {
	var out bytes.Buffer
	input := "4\n\n\n\nsecret-value\nback\n\nno\nyes\n"
	r, err := runProviderWizardPlain(newProviderWizardDraft(""), strings.NewReader(input), &out, nil)
	if err != nil || r.Provider.APIKey != "secret-value" || strings.Contains(out.String(), "secret-value") {
		t.Fatalf("result=%+v err=%v", r, err)
	}
	out.Reset()
	input = "2\n\n\nopenai-secret\nback\nback\nback\nback\n4\n\n\n\n\nno\nyes\n"
	r, err = runProviderWizardPlain(newProviderWizardDraft(""), strings.NewReader(input), &out, nil)
	if err != nil || r.Name != "local" || r.Provider.APIKey != "" || r.Provider.BaseURL != "http://localhost:11434/v1" || strings.Contains(out.String(), "openai-secret") {
		t.Fatalf("switching providers: result=%+v err=%v output=%s", r, err, out.String())
	}
	for _, input := range []string{"", "cancel\n", "2\n\n\nsecret\n"} {
		out.Reset()
		r, err := runProviderWizardPlain(newProviderWizardDraft(""), strings.NewReader(input), &out, nil)
		if !errors.Is(err, errProviderWizardCanceled) || r.Name != "" || r.Provider.APIKey != "" {
			t.Fatalf("%q result=%+v err=%v", input, r, err)
		}
	}
}

func TestProviderWizardRedirectedAndSecretReader(t *testing.T) {
	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "1")
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	in, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := writer.WriteString("2\nwork\n\nscript-secret\nno\nyes\n"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	out, err := os.CreateTemp(dir, "output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	r, err := runProviderWizard("", in, out, false)
	if err != nil || r.Name != "work" || r.Provider.APIKey != "script-secret" {
		t.Fatalf("result=%+v err=%v", r, err)
	}
	text, err := os.ReadFile(out.Name())
	if err != nil || bytes.Contains(text, []byte("script-secret")) || bytes.Contains(text, []byte("\x1b")) {
		t.Fatalf("output=%q err=%v", text, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lintdec")); !os.IsNotExist(err) {
		t.Fatalf("setup wrote config: %v", err)
	}
	var output bytes.Buffer
	calls := 0
	secret := func() (string, error) { calls++; return "tty-secret", nil }
	r, err = runProviderWizardPlain(newProviderWizardDraft(""), strings.NewReader("2\n\n\n\n\n"), &output, secret)
	if err != nil || calls != 1 || r.Provider.APIKey != "tty-secret" || strings.Contains(output.String(), "tty-secret") {
		t.Fatalf("secret reader calls=%d err=%v", calls, err)
	}
}

func TestProviderWizardBooleanSelectionWithoutColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")
	d := newProviderWizardDraft("")
	d.kind = "local"
	f := providerWizardForm(d).WithWidth(80)
	f.Init()
	for i := 0; i < 5; i++ {
		f.NextGroup()
	}
	if !strings.Contains(ansi.Strip(f.GetFocusedField().View()), "> Yes") {
		t.Fatalf("selection not visible without color: %q", ansi.Strip(f.GetFocusedField().View()))
	}
	f.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if d.test || !strings.Contains(ansi.Strip(f.GetFocusedField().View()), "> No") {
		t.Fatalf("changed selection not visible: %q", ansi.Strip(f.GetFocusedField().View()))
	}
}
