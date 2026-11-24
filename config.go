package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/pkg/browser"
	"github.com/zalando/go-keyring"
	"golang.org/x/term"
)

type Provider struct {
	BaseURL            string   `json:"baseUrl"`
	Protocol           string   `json:"protocol,omitempty"` // ""|"openai" or "systemone"
	APIKey             string   `json:"apiKey,omitempty"`
	APIKeyRef          string   `json:"apiKeyRef,omitempty"`
	APIKeyBaseURL      string   `json:"apiKeyBaseUrl,omitempty"`
	APIKeyCreated      bool     `json:"apiKeyCreated,omitempty"`
	Auth               string   `json:"auth,omitempty"`
	OIDCIssuerURL      string   `json:"oidcIssuerUrl,omitempty"`
	OIDCClientID       string   `json:"oidcClientId,omitempty"`
	OIDCResource       string   `json:"oidcResource,omitempty"`
	OIDCScopes         []string `json:"oidcScopes,omitempty"`
	OIDCBaseURL        string   `json:"oidcBaseUrl,omitempty"`
	OIDCTokenFile      string   `json:"oidcTokenFile,omitempty"`
	FederationPolicyID string   `json:"federationPolicyId,omitempty"`
	Models             []string `json:"models,omitempty"`
}

func systemOneRoot(base string) string {
	b := strings.TrimRight(base, "/")
	b = strings.TrimSuffix(b, "/v1/systemone")
	b = strings.TrimSuffix(b, "/v1")
	return b
}

func (p Provider) defaultModel() string {
	if len(p.Models) > 0 {
		return p.Models[0]
	}
	return ""
}

type Config struct {
	Active      string              `json:"active"`
	ActiveModel string              `json:"activeModel,omitempty"`
	Providers   map[string]Provider `json:"providers"`
}

func (c Config) activeModelFor(name string, p Provider) string {
	if name == c.Active && c.ActiveModel != "" {
		return c.ActiveModel
	}
	return p.defaultModel()
}

// findProviderForModel returns the unique provider whose Models list contains model.
func (c Config) findProviderForModel(model string) (string, Provider, bool) {
	var foundName string
	var foundProv Provider
	matches := 0
	for name, p := range c.Providers {
		if slices.Contains(p.Models, model) {
			foundName, foundProv = name, p
			matches++
		}
	}
	return foundName, foundProv, matches == 1
}

func configPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "lintdec", "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "lintdec", "config.json")
}

func loadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(configPath())
	if os.IsNotExist(err) {
		c.Providers = map[string]Provider{}
		return c, nil
	} else if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("bad config %s: %w", configPath(), err)
	}
	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	return c, nil
}

func (c Config) save() error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

var stdinReader = bufio.NewReader(oneByteReader{os.Stdin})

func ask(v *string, label, def string) string {
	if terminal(os.Stdout) {
		color := colorEnabled(true)
		fmt.Printf("  %s", paint(cleanText(label), bold, color))
		if def != "" {
			fmt.Printf(" %s", paint("["+cleanText(def)+"]", dim, color))
		}
		fmt.Printf("  %s ", paint("›", accent, color))
	} else if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		*v = def
	} else {
		*v = line
	}
	return *v
}

type modelCatalogEntry struct {
	ID   string   `json:"id"`
	APIs []string `json:"apis"`
}

func catalogModelIDs(p Provider, entries []modelCatalogEntry) []string {
	var ids []string
	for _, entry := range entries {
		if entry.ID != "" {
			ids = append(ids, entry.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func fetchModels(p Provider) ([]string, error) {
	return fetchModelsContext(context.Background(), p)
}

func fetchModelsContext(ctx context.Context, p Provider) ([]string, error) {
	if p.Protocol == "systemone" {
		return fetchSystemOneModelContext(ctx, p)
	}
	source, err := providerAuth(p)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Timeout: 30 * time.Second}
	base := strings.TrimRight(p.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/models", nil)
	if err != nil {
		return nil, err
	}
	res, err := authorizedDo(c, req, source, p.APIKey)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", responseError(res, source, p.APIKey))
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var m struct {
		Data []modelCatalogEntry `json:"data"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return catalogModelIDs(p, m.Data), nil
}

func fetchSystemOneModel(p Provider) ([]string, error) {
	return fetchSystemOneModelContext(context.Background(), p)
}

func fetchSystemOneModelContext(ctx context.Context, p Provider) ([]string, error) {
	source, err := providerAuth(p)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Timeout: 15 * time.Second}
	base, err := url.Parse(p.BaseURL)
	if err != nil {
		return nil, err
	}
	if base.Hostname() == "openrouter.ai" {
		trusted, ok := trustedHTTPSBase(p.BaseURL)
		if !ok || !openRouterAPIPath(trusted.Path) {
			return nil, fmt.Errorf("OpenRouter discovery requires a trusted HTTPS API endpoint")
		}
		if source == nil {
			source = staticSource(p.APIKey)
		}
		return fetchJevModels(ctx, systemOneRoot(p.BaseURL)+"/v1", source, c)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", systemOneRoot(p.BaseURL)+"/health", nil)
	if err != nil {
		return nil, err
	}
	res, err := authorizedDo(c, req, source, p.APIKey)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", responseError(res, source, p.APIKey))
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var h struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, healthNotDecider(p.BaseURL, err)
	}
	if h.Model == "" {
		return []string{"strands-decider-latest"}, nil
	}
	return []string{h.Model}, nil
}

// healthNotDecider explains the common protocol mixup: OpenAI servers answer /health
// with plain text, so a JSON parse failure means the provider was added as systemone.
func healthNotDecider(baseURL string, err error) error {
	return fmt.Errorf("%s: /health is not Strands Decider JSON (%v); for an OpenAI server use protocol openai", baseURL, err)
}

func testConnection(p Provider, model string) error {
	return testConnectionContext(context.Background(), p, model)
}

func testConnectionContext(ctx context.Context, p Provider, model string) error {
	if model == "" {
		model = p.defaultModel()
	}
	ids, err := fetchModelsContext(ctx, p)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("provider returned no models")
	}
	if model == "" {
		fmt.Printf("connected; %d models available\n", len(ids))
	} else if !slices.Contains(ids, model) {
		fmt.Printf("connected; %d models available — note: %q is not listed\n", len(ids), model)
	} else {
		fmt.Printf("connected; %s is available (%d models total)\n", model, len(ids))
	}
	return nil
}

// openBrowser is a variable so tests can never launch a real browser.
var openBrowser = browser.OpenURL

func openLoginPage(rawURL string) error {
	fmt.Printf("Open this link on this machine to sign in. Do not share it:\n%s\n", rawURL)
	go func() {
		if err := openBrowser(rawURL); err != nil {
			fmt.Fprintln(os.Stderr, "Could not open a browser; use the link above.")
		}
	}()
	return nil
}

func providerAdd(c *Config, rest []string) int {
	if terminal(os.Stdout) {
		color := colorEnabled(true)
		fmt.Printf("\n  %s\n  %s\n\n", paint("ADD A PROVIDER", bold, color), paint("Connect an endpoint, then choose a model.", dim, color))
	}
	result, err := readProviderWizard(rest)
	if err != nil {
		if errors.Is(err, errProviderWizardCanceled) {
			fmt.Fprintln(os.Stderr, "provider setup canceled")
			return 130
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if _, exists := c.Providers[result.Name]; exists {
		fmt.Fprintf(os.Stderr, "provider %q already exists\n", result.Name)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	s := &providerSetup{ctx: ctx, result: result, p: result.Provider}
	defer s.cleanup()
	if result.BrowserOpenRouter {
		if err := s.signInOpenRouter(); err != nil {
			return s.fail(err)
		}
	}
	if err := s.authenticate(); err != nil {
		return s.fail(err)
	}
	tested, code := s.testConnection()
	if code != 0 {
		return code
	}
	if ctx.Err() != nil {
		return 130
	}
	return s.save(c, tested)
}

// readProviderWizard runs the form on a terminal and the plain prompts for scripts.
func readProviderWizard(rest []string) (providerWizardResult, error) {
	defName := ""
	if len(rest) > 0 {
		defName = rest[0]
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return runProviderWizard(defName, os.Stdin, os.Stdout, os.Getenv("LINTDEC_ACCESSIBLE") == "1")
	}
	return runProviderWizardPlain(newProviderWizardDraft(defName), stdinReader, os.Stdout, nil)
}

// providerSetup tracks what an unsaved setup must undo or warn about.
type providerSetup struct {
	ctx                                   context.Context
	result                                providerWizardResult
	p                                     Provider
	ref                                   string
	saved, oidcSignedIn, browserKeyIssued bool
}

func (s *providerSetup) cleanup() {
	if s.saved {
		return
	}
	if s.ref != "" {
		cleanupNewAPIKey(s.ref)
	}
	if s.browserKeyIssued {
		fmt.Fprintln(os.Stderr, "OpenRouter created an API key; revoke it at https://openrouter.ai/settings/keys if unused.")
	}
	if s.oidcSignedIn {
		fmt.Fprintln(os.Stderr, "OIDC sign-in succeeded but the provider was not saved; its shared keychain session remains available.")
	}
}

// fail prints err and returns the exit code: 130 when the user interrupted.
func (s *providerSetup) fail(err error) int {
	fmt.Fprintln(os.Stderr, err)
	if s.ctx.Err() != nil {
		return 130
	}
	return 2
}

func (s *providerSetup) signInOpenRouter() error {
	u, ok := trustedHTTPSBase(s.p.BaseURL)
	if !ok || u.Hostname() != "openrouter.ai" || !openRouterAPIPath(u.Path) {
		return errors.New("OpenRouter sign-in requires its HTTPS API endpoint")
	}
	key, err := loginOpenRouterOAuth(s.ctx, openLoginPage, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("OpenRouter sign-in failed: %w", err)
	}
	s.p.APIKey, s.p.APIKeyCreated, s.browserKeyIssued = key, true, true
	return nil
}

// authenticate moves a new API key into the keychain and completes browser OIDC sign-in.
func (s *providerSetup) authenticate() error {
	var err error
	if s.ref, err = prepareProviderAPIKey(&s.p, keyring.Set); err != nil {
		return err
	}
	if s.p.Auth == "oidc" {
		s.p.OIDCIssuerURL, s.p.OIDCClientID = s.result.OIDCIssuerURL, s.result.OIDCClientID
		s.p.OIDCResource, s.p.OIDCScopes, s.p.OIDCBaseURL = s.result.OIDCResource, s.result.OIDCScopes, s.p.BaseURL
	}
	source, err := providerAuth(s.p)
	if err != nil || s.p.Auth != "oidc" {
		return err
	}
	browser, ok := source.(*OIDCBrowser)
	if !ok {
		return errors.New("OIDC browser source unavailable")
	}
	if err := browser.Login(s.ctx, openLoginPage); err != nil {
		return fmt.Errorf("OIDC sign-in failed: %w", err)
	}
	s.oidcSignedIn = true
	return nil
}

// testConnection reports whether the endpoint answered; the user may keep one that did not.
func (s *providerSetup) testConnection() (tested bool, code int) {
	if !s.result.Test {
		return false, 0
	}
	err := testConnectionContext(s.ctx, s.p, "")
	if err == nil {
		return true, 0
	}
	if s.ctx.Err() != nil {
		return false, 130
	}
	fmt.Fprintf(os.Stderr, "connection test failed: %v\n", err)
	answer := make(chan string, 1)
	go func() { var k string; answer <- ask(&k, "add it anyway?", "n") }()
	select {
	case k := <-answer:
		if !strings.HasPrefix(strings.ToLower(k), "y") {
			return false, 2
		}
		return false, 0
	case <-s.ctx.Done():
		return false, 130
	}
}

func (s *providerSetup) save(c *Config, tested bool) int {
	name := s.result.Name
	c.Providers[name] = s.p
	if s.result.Active || c.Active == "" {
		c.Active = name
		c.ActiveModel = ""
	}
	if err := c.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	s.saved = true
	suffix := ""
	if c.Active == name {
		suffix = " (active)"
	}
	fmt.Printf("added %s%s\n", name, suffix)
	// A provider with no model is unusable for linting, so pick one right away.
	if tested && c.Active == name {
		return modelPick(c, []string{name})
	}
	fmt.Printf("run 'lintdec model %s' to choose a model\n", name)
	return 0
}

func providerList(c Config) int {
	if len(c.Providers) == 0 {
		fmt.Println("no providers configured; add one: lintdec provider add")
		return 0
	}
	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	slices.Sort(names)
	width := 12
	for _, name := range names {
		width = max(width, runewidth.StringWidth(cleanText(name)))
	}
	interactive := terminal(os.Stdout)
	if interactive {
		fmt.Printf("\n  %s\n\n", paint("PROVIDERS", bold, colorEnabled(true)))
	}
	for _, name := range names {
		p := c.Providers[name]
		mark := " "
		if name == c.Active {
			mark = "*"
		}
		proto := ""
		if p.Protocol != "" {
			proto = " [" + p.Protocol + "]"
		}
		fmt.Printf("%s %s %s%s\n", paint(mark, accent, colorEnabled(interactive)), padText(name, width), cleanText(p.BaseURL), cleanText(proto))
		activeModel := c.activeModelFor(name, p)
		for _, m := range p.Models {
			mMark := " "
			if name == c.Active && m == activeModel {
				mMark = "*"
			}
			fmt.Printf("    %s %s\n", paint(mMark, accent, colorEnabled(interactive)), cleanText(m))
		}
	}
	return 0
}

func providerPick(c *Config) int {
	if len(c.Providers) == 0 {
		return providerAdd(c, nil)
	}
	names := make([]string, 0, len(c.Providers))
	width := 8
	for name := range c.Providers {
		names = append(names, name)
		width = max(width, runewidth.StringWidth(cleanText(name)))
	}
	slices.Sort(names)
	color := colorEnabled(terminal(os.Stdout))
	fmt.Printf("\n  %s\n\n", paint("CHOOSE A PROVIDER", bold, color))
	defaultChoice := ""
	for i, name := range names {
		mark, suffix := " ", ""
		if name == c.Active {
			mark, suffix, defaultChoice = "*", "  active", strconv.Itoa(i+1)
		}
		fmt.Printf("  %s %2d  %s  %s%s\n", paint(mark, accent, color), i+1, padText(name, width), cleanText(c.Providers[name].BaseURL), paint(suffix, dim, color))
	}
	fmt.Printf("    a   %s\n\n", paint("Add a provider", accent, color))
	var raw string
	ask(&raw, "pick number or a", defaultChoice)
	if strings.EqualFold(raw, "a") {
		return providerAdd(c, nil)
	}
	index, err := strconv.Atoi(raw)
	if err != nil || index < 1 || index > len(names) {
		fmt.Fprintf(os.Stderr, "invalid selection: %q\n", raw)
		return 2
	}
	if names[index-1] == c.Active {
		return 0
	}
	return providerUse(c, []string{names[index-1]})
}

func providerUse(c *Config, rest []string) int {
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: lintdec provider use <name> [model]")
		return 2
	}
	p, ok := c.Providers[rest[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "no such provider: %s\n", rest[0])
		return 2
	}
	c.Active = rest[0]
	if len(rest) > 1 {
		c.ActiveModel = rest[1]
		if !slices.Contains(p.Models, rest[1]) {
			p.Models = append(p.Models, rest[1])
			c.Providers[rest[0]] = p
		}
	} else {
		c.ActiveModel = p.defaultModel()
	}
	if err := c.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if c.ActiveModel != "" {
		fmt.Printf("active provider: %s (model %s)\n", c.Active, c.ActiveModel)
	} else {
		fmt.Printf("active provider: %s\n", c.Active)
	}
	return 0
}

func cleanupNewAPIKey(ref string) {
	if err := keyring.Delete(apiKeyKeychainService, ref); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		fmt.Fprintln(os.Stderr, "could not remove the unused API key from the OS keychain")
	}
}

func providerRm(c *Config, rest []string) int {
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: lintdec provider rm <name>")
		return 2
	}
	p, ok := c.Providers[rest[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "no such provider: %s\n", rest[0])
		return 2
	}
	if c.Active == rest[0] && !slices.Contains(rest[1:], "--force") {
		fmt.Fprintf(os.Stderr, "%s is active; use --force\n", rest[0])
		return 2
	}
	delete(c.Providers, rest[0])
	if c.Active == rest[0] {
		c.Active = ""
		c.ActiveModel = ""
	}
	if err := c.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := deleteUnusedAPIKey(*c, p, keyring.Delete); err != nil {
		fmt.Fprintln(os.Stderr, "provider removed, but its API key could not be removed from the OS keychain")
	}
	if p.Auth == "oidc" {
		fmt.Fprintln(os.Stderr, "The shared OIDC session remains in the OS keychain; revoke it with your identity provider if unused.")
	}
	if p.APIKeyCreated {
		fmt.Fprintln(os.Stderr, "The OpenRouter API key remains active; revoke it at https://openrouter.ai/settings/keys if unused.")
	}
	fmt.Printf("removed %s\n", rest[0])
	return 0
}

func providerLogin(c Config, rest []string) int {
	name := c.Active
	if len(rest) > 0 {
		name = rest[0]
	}
	p, ok := c.Providers[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "no such provider: %s\n", name)
		return 2
	}
	if p.Auth != "oidc" {
		fmt.Fprintln(os.Stderr, "browser login is only available for OIDC providers")
		return 2
	}
	source, err := providerAuth(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	browser, ok := source.(*OIDCBrowser)
	if !ok {
		fmt.Fprintln(os.Stderr, "OIDC browser source unavailable")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := browser.Login(ctx, openLoginPage); err != nil {
		fmt.Fprintf(os.Stderr, "OIDC sign-in failed: %v\n", err)
		if ctx.Err() != nil {
			return 130
		}
		return 2
	}
	fmt.Printf("signed in to %s\n", cleanText(name))
	return 0
}

func providerTest(c Config, rest []string) int {
	name := c.Active
	if len(rest) > 0 {
		name = rest[0]
	}
	p, ok := c.Providers[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "no such provider: %s\n", name)
		return 2
	}
	model := c.activeModelFor(name, p)
	if len(rest) > 1 {
		model = rest[1]
	}
	if err := testConnection(p, model); err != nil {
		fmt.Fprintf(os.Stderr, "connection test failed: %v\n", err)
		return 2
	}
	return 0
}

func providerCmd(args []string) int {
	c, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if len(args) == 0 && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		return providerPick(&c)
	}
	sub, rest := "list", []string{}
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "list", "":
		return providerList(c)
	case "add":
		return providerAdd(&c, rest)
	case "use":
		return providerUse(&c, rest)
	case "rm":
		return providerRm(&c, rest)
	case "test":
		return providerTest(c, rest)
	case "login":
		return providerLogin(c, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s (list|add|use|rm|test|login)\n", sub)
		return 2
	}
}
