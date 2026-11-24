package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/term"
)

// providerWizardResult is a proposal; the caller handles browser auth, testing, and saving.
type providerWizardResult struct {
	Name              string
	Provider          Provider
	OIDCIssuerURL     string
	OIDCClientID      string
	OIDCResource      string
	OIDCScopes        []string
	BrowserOpenRouter bool
	Test              bool
	Active            bool
}

var errProviderWizardCanceled = errors.New("provider setup canceled")

type providerWizardDraft struct {
	kind, name, protocol, routerAuth, issue                string
	routerURL, openaiURL, oidcURL, localURL                string
	openaiKey, routerKey, localKey                         string
	routerIssuer, routerClientID, oidcIssuer, oidcClientID string
	oidcResource, oidcScopes, tokenFile, policy            string
	routerAdvanced, test, active                           bool
}

func newProviderWizardDraft(name string) *providerWizardDraft {
	return &providerWizardDraft{kind: "openrouter", name: name, protocol: "openai", routerAuth: "api-key", test: true, active: true}
}

func (d *providerWizardDraft) defaultName() string {
	switch d.kind {
	case "openrouter", "openai", "custom-oidc":
		return d.kind
	default:
		return "local"
	}
}

func (d *providerWizardDraft) endpoint() *string {
	switch d.kind {
	case "openrouter":
		return &d.routerURL
	case "openai":
		return &d.openaiURL
	case "custom-oidc":
		return &d.oidcURL
	default:
		return &d.localURL
	}
}

func (d *providerWizardDraft) apiKey() *string {
	switch d.kind {
	case "openai":
		return &d.openaiKey
	case "openrouter":
		return &d.routerKey
	default:
		return &d.localKey
	}
}

func (d *providerWizardDraft) issuer() *string {
	if d.kind == "openrouter" {
		return &d.routerIssuer
	}
	return &d.oidcIssuer
}

func (d *providerWizardDraft) clientID() *string {
	if d.kind == "openrouter" {
		return &d.routerClientID
	}
	return &d.oidcClientID
}

func (d *providerWizardDraft) quickOpenRouter() bool {
	return d.kind == "openrouter" && !d.routerAdvanced
}

func (d *providerWizardDraft) auth() string {
	switch d.kind {
	case "openrouter":
		if d.quickOpenRouter() {
			return "openrouter-browser"
		}
		return d.routerAuth
	case "custom-oidc":
		return "oidc"
	default:
		return "api-key"
	}
}

func (d *providerWizardDraft) defaultEndpoint() string {
	switch d.kind {
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	case "openai":
		return "https://api.openai.com/v1"
	case "custom-oidc":
		return ""
	default:
		if d.protocol == "systemone" {
			return "http://localhost:8000"
		}
		return "http://localhost:11434/v1"
	}
}

func (d *providerWizardDraft) defaultIssuer() string {
	endpoint := firstNonEmpty(strings.TrimSpace(*d.endpoint()), d.defaultEndpoint())
	if u, err := url.Parse(endpoint); err == nil && strings.EqualFold(u.Hostname(), "openrouter.ai") {
		return "https://clerk.openrouter.ai"
	}
	return ""
}

func (d *providerWizardDraft) result() providerWizardResult {
	p := Provider{BaseURL: strings.TrimRight(firstNonEmpty(strings.TrimSpace(*d.endpoint()), d.defaultEndpoint()), "/")}
	if d.quickOpenRouter() {
		p.BaseURL = d.defaultEndpoint()
	}
	if d.quickOpenRouter() || (d.kind != "openai" && d.protocol == "systemone") {
		p.Protocol = "systemone"
	}
	auth := d.auth()
	if auth != "api-key" && auth != "openrouter-browser" {
		p.Auth = auth
	}
	switch auth {
	case "api-key":
		p.APIKey = *d.apiKey()
	case "openrouter-oidc":
		p.OIDCTokenFile = firstNonEmpty(strings.TrimSpace(d.tokenFile), os.Getenv("OPENROUTER_OIDC_TOKEN_FILE"))
		p.FederationPolicyID = firstNonEmpty(strings.TrimSpace(d.policy), os.Getenv("OPENROUTER_FEDERATION_POLICY_ID"))
	}
	r := providerWizardResult{Name: firstNonEmpty(strings.TrimSpace(d.name), d.defaultName()), Provider: p, BrowserOpenRouter: auth == "openrouter-browser", Test: d.test || d.quickOpenRouter(), Active: d.active || d.quickOpenRouter()}
	if auth == "oidc" {
		r.OIDCIssuerURL = firstNonEmpty(strings.TrimSpace(*d.issuer()), d.defaultIssuer())
		r.OIDCClientID = strings.TrimSpace(*d.clientID())
		if d.kind == "openrouter" {
			r.OIDCResource = "https://openrouter.ai/api/v1"
			r.OIDCScopes = []string{"offline_access", "email", "inference"}
		} else {
			r.OIDCResource = firstNonEmpty(strings.TrimSpace(d.oidcResource), p.BaseURL)
			r.OIDCScopes = strings.Fields(firstNonEmpty(strings.TrimSpace(d.oidcScopes), "offline_access"))
		}
	}
	return r
}

func (d *providerWizardDraft) validateEndpoint(endpoint string) error {
	base := strings.TrimRight(firstNonEmpty(strings.TrimSpace(endpoint), d.defaultEndpoint()), "/")
	u, err := url.Parse(base)
	if err != nil || !bareHTTPURL(u) {
		return errors.New("enter an HTTP(S) base URL without credentials, query, or fragment")
	}
	if d.auth() == "oidc" && u.Scheme != "https" {
		return errors.New("browser OIDC requires an HTTPS base URL")
	}
	if d.kind == "openrouter" {
		trusted, ok := trustedHTTPSBase(base)
		if !ok || trusted.Hostname() != "openrouter.ai" || !openRouterAPIPath(trusted.Path) {
			return errors.New("OpenRouter requires the trusted HTTPS OpenRouter API endpoint")
		}
	}
	if d.auth() == "openrouter-oidc" {
		p := Provider{BaseURL: base, Auth: d.auth(), OIDCTokenFile: "placeholder", FederationPolicyID: "placeholder"}
		_, err = providerAuth(p)
		return err
	}
	return nil
}

func bareHTTPURL(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func wizardRequired(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("this field is required")
	}
	return nil
}

func (d *providerWizardDraft) validate() error {
	if err := wizardRequired(firstNonEmpty(strings.TrimSpace(d.name), d.defaultName())); err != nil {
		return fmt.Errorf("provider name: %w", err)
	}
	endpoint := *d.endpoint()
	if d.quickOpenRouter() {
		endpoint = ""
	}
	if err := d.validateEndpoint(endpoint); err != nil {
		return fmt.Errorf("base URL: %w", err)
	}
	switch d.auth() {
	case "api-key":
		if d.kind == "openai" || d.kind == "openrouter" {
			if err := wizardRequired(*d.apiKey()); err != nil {
				return fmt.Errorf("API key: %w", err)
			}
		}
	case "oidc":
		if err := d.validateIssuer(*d.issuer()); err != nil {
			return err
		}
		if err := wizardRequired(*d.clientID()); err != nil {
			return fmt.Errorf("OIDC public client ID: %w", err)
		}
	case "openrouter-oidc":
		if err := wizardRequired(firstNonEmpty(strings.TrimSpace(d.tokenFile), os.Getenv("OPENROUTER_OIDC_TOKEN_FILE"))); err != nil {
			return fmt.Errorf("OIDC subject-token file: %w", err)
		}
		if err := wizardRequired(firstNonEmpty(strings.TrimSpace(d.policy), os.Getenv("OPENROUTER_FEDERATION_POLICY_ID"))); err != nil {
			return fmt.Errorf("OpenRouter federation policy ID: %w", err)
		}
	}
	return nil
}

func (d *providerWizardDraft) validateIssuer(issuer string) error {
	issuer = firstNonEmpty(strings.TrimSpace(issuer), d.defaultIssuer())
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("OIDC issuer: enter an HTTPS issuer URL without credentials, query, or fragment")
	}
	return nil
}

// runProviderWizard never saves or performs network requests. accessible selects
// the plain, back-capable prompts; redirected streams and TERM=dumb do so too.
func runProviderWizard(name string, in, out *os.File, accessible bool) (providerWizardResult, error) {
	d := newProviderWizardDraft(name)
	if accessible || !terminal(in) || !terminal(out) {
		var secret func() (string, error)
		if term.IsTerminal(int(in.Fd())) {
			secret = func() (string, error) {
				b, err := term.ReadPassword(int(in.Fd()))
				fmt.Fprintln(out)
				return string(b), err
			}
		}
		return runProviderWizardPlain(d, in, out, secret)
	}
	for {
		form := providerWizardForm(d)
		if !colorEnabled(true) {
			form.WithProgramOptions(tea.WithColorProfile(colorprofile.Ascii))
		}
		form.WithInput(in).WithOutput(out)
		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return providerWizardResult{}, errProviderWizardCanceled
			}
			return providerWizardResult{}, err
		}
		if form.State != huh.StateCompleted {
			return providerWizardResult{}, errProviderWizardCanceled
		}
		if err := d.validate(); err != nil {
			d.issue = "Setup not submitted: " + err.Error() + ". Please correct it; prior values are retained."
			fmt.Fprintln(out, d.issue)
			continue
		}
		return d.result(), nil
	}
}

func providerWizardForm(d *providerWizardDraft) *huh.Form {
	groups := []*huh.Group{
		huh.NewGroup(huh.NewSelect[string]().Title("Provider").Description(firstNonEmpty(d.issue, "Choose a provider; local/custom also supports servers without an API key.")).
			Options(huh.NewOption("OpenRouter", "openrouter"),
				huh.NewOption("OpenAI", "openai"), huh.NewOption("Custom OIDC", "custom-oidc"),
				huh.NewOption("Local/custom endpoint", "local")).Value(&d.kind)),
		huh.NewGroup(huh.NewInput().Title("Provider name").Description("Leave blank to use the provider name.").
			PlaceholderFunc(d.defaultName, &d.kind).Value(&d.name)),
		huh.NewGroup(huh.NewSelect[bool]().Title("OpenRouter setup").Description("Quick setup uses Jev and signs in with your browser, creating an API key.").
			Options(huh.NewOption("Quick setup", false), huh.NewOption("Advanced setup", true)).Value(&d.routerAdvanced)).
			WithHideFunc(func() bool { return d.kind != "openrouter" }),
		huh.NewGroup(huh.NewSelect[string]().Title("Model type").Description("Choose chat models or decision models such as Jev.").
			Options(huh.NewOption("Chat models (OpenAI-compatible)", "openai"), huh.NewOption("Decision models (System One, including Jev)", "systemone")).Value(&d.protocol)).
			WithHideFunc(func() bool { return d.kind == "openai" || d.quickOpenRouter() }),
		huh.NewGroup(huh.NewSelect[string]().Title("OpenRouter authentication").Description("Use an existing API key, sign in to create one, or choose an OIDC method.").
			Options(huh.NewOption("API key", "api-key"), huh.NewOption("Sign in with browser (creates an API key)", "openrouter-browser"), huh.NewOption("Browser OIDC (requires client ID)", "oidc"), huh.NewOption("Workload identity", "openrouter-oidc")).Value(&d.routerAuth)).
			WithHideFunc(func() bool { return d.kind != "openrouter" || d.quickOpenRouter() }),
	}
	for _, endpoint := range []struct {
		kind  string
		value *string
	}{
		{"openrouter", &d.routerURL}, {"openai", &d.openaiURL}, {"custom-oidc", &d.oidcURL}, {"local", &d.localURL},
	} {
		description := "Leave blank to use the shown default."
		if endpoint.kind == "custom-oidc" {
			description = "Enter the HTTPS endpoint that accepts this identity."
		}
		groups = append(groups, huh.NewGroup(huh.NewInput().Title("Base URL").Description(description).
			PlaceholderFunc(d.defaultEndpoint, &struct{ Kind, Protocol *string }{&d.kind, &d.protocol}).Value(endpoint.value)).
			WithHideFunc(func() bool { return d.kind != endpoint.kind || d.quickOpenRouter() }))
	}
	for _, key := range []struct {
		kind  string
		value *string
	}{{"openai", &d.openaiKey}, {"openrouter", &d.routerKey}, {"local", &d.localKey}} {
		title := "API key (blank if none)"
		if key.kind == "openai" {
			title = "OpenAI API key (required)"
		} else if key.kind == "openrouter" {
			title = "OpenRouter API key (required)"
		}
		groups = append(groups, huh.NewGroup(huh.NewInput().Title(title).Description("Input is masked and stored in the OS keychain.").EchoMode(huh.EchoModePassword).Value(key.value)).
			WithHideFunc(func() bool { return d.kind != key.kind || d.auth() != "api-key" }))
	}
	for _, oidc := range []struct {
		kind             string
		issuer, clientID *string
	}{{"openrouter", &d.routerIssuer, &d.routerClientID}, {"custom-oidc", &d.oidcIssuer, &d.oidcClientID}} {
		hide := func() bool { return d.kind != oidc.kind || d.auth() != "oidc" }
		groups = append(groups,
			huh.NewGroup(huh.NewInput().Title("OIDC issuer URL").Description("HTTPS issuer for discovery; leave blank to use the shown default.").
				PlaceholderFunc(d.defaultIssuer, &struct{ Kind, Endpoint *string }{&d.kind, oidcEndpoint(d, oidc.kind)}).Value(oidc.issuer)).WithHideFunc(hide),
			huh.NewGroup(huh.NewInput().Title("OIDC public client ID").Description("Enter the public client ID registered with the identity provider.").Value(oidc.clientID)).WithHideFunc(hide))
	}
	groups = append(groups,
		huh.NewGroup(huh.NewInput().Title("OIDC resource / audience").Description("Optional API resource; blank uses the endpoint URL.").
			PlaceholderFunc(func() string { return strings.TrimRight(strings.TrimSpace(d.oidcURL), "/") }, &d.oidcURL).Value(&d.oidcResource)).
			WithHideFunc(func() bool { return d.kind != "custom-oidc" }),
		huh.NewGroup(huh.NewInput().Title("OIDC scopes").Description("Space-separated scopes; blank uses offline_access.").Placeholder("offline_access").Value(&d.oidcScopes)).
			WithHideFunc(func() bool { return d.kind != "custom-oidc" }),
		huh.NewGroup(huh.NewInput().Title("OIDC subject-token file").Description("Rotating workload token; blank uses OPENROUTER_OIDC_TOKEN_FILE.").Value(&d.tokenFile)).
			WithHideFunc(func() bool { return d.auth() != "openrouter-oidc" }),
		huh.NewGroup(huh.NewInput().Title("OpenRouter federation policy ID").Description("Blank uses OPENROUTER_FEDERATION_POLICY_ID.").Value(&d.policy)).
			WithHideFunc(func() bool { return d.auth() != "openrouter-oidc" }),
		huh.NewGroup(huh.NewSelect[bool]().Title("Test connection after setup?").Description("The caller performs the test; the form makes no network requests.").
			Options(huh.NewOption("Yes", true), huh.NewOption("No", false)).Value(&d.test)).WithHideFunc(d.quickOpenRouter),
		huh.NewGroup(huh.NewSelect[bool]().Title("Set as active?").
			Options(huh.NewOption("Yes", true), huh.NewOption("No", false)).Value(&d.active)).WithHideFunc(d.quickOpenRouter))
	return huh.NewForm(groups...).WithShowHelp(true)
}

func oidcEndpoint(d *providerWizardDraft, kind string) *string {
	if kind == "openrouter" {
		return &d.routerURL
	}
	return &d.oidcURL
}

// oneByteReader prevents buffering future terminal input before ReadPassword.
type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

// plainStep is one prompt of the plain flow; blank means an empty answer stays empty instead of taking the default.
type plainStep struct {
	skip          func(d *providerWizardDraft) bool
	ask           func(d *providerWizardDraft) (prompt, def string)
	set           func(d *providerWizardDraft, value string) error
	blank, secret bool
}

func never(*providerWizardDraft) bool { return false }

func notKind(kind string) func(*providerWizardDraft) bool {
	return func(d *providerWizardDraft) bool { return d.kind != kind }
}

func notAuth(auth string) func(*providerWizardDraft) bool {
	return func(d *providerWizardDraft) bool { return d.auth() != auth }
}

func quick(d *providerWizardDraft) bool { return d.quickOpenRouter() }

// required stores a non-blank answer through field.
func required(field func(*providerWizardDraft) *string) func(*providerWizardDraft, string) error {
	return func(d *providerWizardDraft, value string) error {
		if err := wizardRequired(value); err != nil {
			return err
		}
		*field(d) = value
		return nil
	}
}

// choice stores the canonical name of a numbered option through field.
func choice(field func(*providerWizardDraft) *string, options ...[]string) func(*providerWizardDraft, string) error {
	return func(d *providerWizardDraft, value string) error {
		v, err := choose(value, options...)
		if err == nil {
			*field(d) = v
		}
		return err
	}
}

// choose maps a typed number or name to its option; each option lists its accepted names, canonical first.
func choose(value string, options ...[]string) (string, error) {
	v := strings.ToLower(value)
	nums := make([]string, len(options))
	for i, names := range options {
		nums[i] = strconv.Itoa(i + 1)
		if v == nums[i] || slices.Contains(names, v) {
			return names[0], nil
		}
	}
	if len(nums) == 2 {
		return "", errors.New("choose 1 or 2")
	}
	return "", fmt.Errorf("choose %s, or %s", strings.Join(nums[:len(nums)-1], ", "), nums[len(nums)-1])
}

func yesNo(field func(*providerWizardDraft) *bool) func(*providerWizardDraft, string) error {
	return func(d *providerWizardDraft, value string) error {
		switch strings.ToLower(value) {
		case "y", "yes":
			*field(d) = true
		case "n", "no":
			*field(d) = false
		default:
			return errors.New("enter yes or no")
		}
		return nil
	}
}

var plainSteps = []plainStep{
	{skip: never, ask: func(d *providerWizardDraft) (string, string) {
		return "Provider: 1 OpenRouter, 2 OpenAI, 3 custom-oidc, 4 local", d.kind
	}, set: choice(func(d *providerWizardDraft) *string { return &d.kind }, []string{"openrouter"}, []string{"openai"}, []string{"custom-oidc"}, []string{"local"})},
	{skip: never, blank: true, ask: func(d *providerWizardDraft) (string, string) {
		return "Provider name", firstNonEmpty(d.name, d.defaultName())
	}, set: func(d *providerWizardDraft, value string) error {
		d.name = firstNonEmpty(value, d.name)
		return nil
	}},
	{skip: notKind("openrouter"), ask: func(d *providerWizardDraft) (string, string) {
		def := "quick"
		if d.routerAdvanced {
			def = "advanced"
		}
		return "OpenRouter setup: 1 quick (Jev and browser sign-in, creates an API key), 2 advanced", def
	}, set: func(d *providerWizardDraft, value string) error {
		v, err := choose(value, []string{"quick"}, []string{"advanced"})
		if err == nil {
			d.routerAdvanced = v == "advanced"
		}
		return err
	}},
	{skip: func(d *providerWizardDraft) bool { return d.kind == "openai" || d.quickOpenRouter() }, ask: func(d *providerWizardDraft) (string, string) {
		return "Model type: 1 openai (chat), 2 systemone (decisions including Jev)", d.protocol
	}, set: choice(func(d *providerWizardDraft) *string { return &d.protocol }, []string{"openai"}, []string{"systemone"})},
	{skip: func(d *providerWizardDraft) bool { return d.kind != "openrouter" || d.quickOpenRouter() }, ask: func(d *providerWizardDraft) (string, string) {
		return "OpenRouter authentication: 1 api-key, 2 browser (creates an API key), 3 oidc (requires client ID), 4 openrouter-oidc (workload identity)", d.routerAuth
	}, set: choice(func(d *providerWizardDraft) *string { return &d.routerAuth },
		[]string{"api-key"}, []string{"openrouter-browser", "browser"}, []string{"oidc"}, []string{"openrouter-oidc"})},
	{skip: quick, ask: func(d *providerWizardDraft) (string, string) {
		return "Base URL", firstNonEmpty(*d.endpoint(), d.defaultEndpoint())
	}, set: func(d *providerWizardDraft, value string) error {
		if value == d.defaultEndpoint() {
			value = ""
		}
		if err := d.validateEndpoint(value); err != nil {
			return err
		}
		*d.endpoint() = value
		return nil
	}},
	{skip: notAuth("api-key"), blank: true, secret: true, ask: apiKeyPrompt, set: setAPIKey},
	{skip: notAuth("oidc"), ask: func(d *providerWizardDraft) (string, string) {
		return "OIDC issuer URL (HTTPS)", firstNonEmpty(*d.issuer(), d.defaultIssuer())
	}, set: func(d *providerWizardDraft, value string) error {
		if value == d.defaultIssuer() {
			value = ""
		}
		if err := d.validateIssuer(value); err != nil {
			return err
		}
		*d.issuer() = value
		return nil
	}},
	{skip: notAuth("oidc"), ask: func(d *providerWizardDraft) (string, string) {
		return "OIDC public client ID", *d.clientID()
	}, set: required((*providerWizardDraft).clientID)},
	{skip: notKind("custom-oidc"), ask: func(d *providerWizardDraft) (string, string) {
		return "OIDC resource / audience (blank uses endpoint URL)", firstNonEmpty(d.oidcResource, strings.TrimRight(strings.TrimSpace(d.oidcURL), "/"))
	}, set: func(d *providerWizardDraft, value string) error {
		if value == strings.TrimRight(strings.TrimSpace(d.oidcURL), "/") {
			value = ""
		}
		d.oidcResource = value
		return nil
	}},
	{skip: notKind("custom-oidc"), ask: func(d *providerWizardDraft) (string, string) {
		return "OIDC scopes (space-separated)", firstNonEmpty(d.oidcScopes, "offline_access")
	}, set: func(d *providerWizardDraft, value string) error {
		if value == "offline_access" {
			value = ""
		}
		d.oidcScopes = value
		return nil
	}},
	{skip: notAuth("openrouter-oidc"), ask: func(d *providerWizardDraft) (string, string) {
		return "OIDC subject-token file", firstNonEmpty(d.tokenFile, os.Getenv("OPENROUTER_OIDC_TOKEN_FILE"))
	}, set: required(func(d *providerWizardDraft) *string { return &d.tokenFile })},
	{skip: notAuth("openrouter-oidc"), ask: func(d *providerWizardDraft) (string, string) {
		return "OpenRouter federation policy ID", firstNonEmpty(d.policy, os.Getenv("OPENROUTER_FEDERATION_POLICY_ID"))
	}, set: required(func(d *providerWizardDraft) *string { return &d.policy })},
	{skip: quick, ask: func(d *providerWizardDraft) (string, string) {
		return "Test connection after setup? (yes/no; caller performs test)", wizardYesNo(d.test)
	}, set: yesNo(func(d *providerWizardDraft) *bool { return &d.test })},
	{skip: quick, ask: func(d *providerWizardDraft) (string, string) {
		return "Set as active? (yes/no)", wizardYesNo(d.active)
	}, set: yesNo(func(d *providerWizardDraft) *bool { return &d.active })},
}

func apiKeyPrompt(d *providerWizardDraft) (string, string) {
	switch {
	case *d.apiKey() != "":
		return "API key (blank keeps existing; enter clear to remove; input hidden on a TTY)", ""
	case d.kind == "openai":
		return "OpenAI API key (required; input hidden on a TTY)", ""
	case d.kind == "openrouter":
		return "OpenRouter API key (required; input hidden on a TTY)", ""
	}
	return "API key (blank if none; input hidden on a TTY)", ""
}

func setAPIKey(d *providerWizardDraft, value string) error {
	if strings.EqualFold(value, "clear") && *d.apiKey() != "" {
		*d.apiKey() = ""
	} else if value != "" {
		*d.apiKey() = value
	}
	if d.kind == "openai" || d.kind == "openrouter" {
		return wizardRequired(*d.apiKey())
	}
	return nil
}

// The plain flow accepts choice names or numbers, and reserves back/cancel as commands.
// A nil secret reader is for scripts: input is not echoed by this component.
func runProviderWizardPlain(d *providerWizardDraft, in io.Reader, out io.Writer, secret func() (string, error)) (providerWizardResult, error) {
	r := bufio.NewReader(oneByteReader{in})
	fmt.Fprintln(out, "Provider setup: enter back to revisit a field or cancel to exit.")
	for step := 0; step < len(plainSteps); {
		st := plainSteps[step]
		if st.skip(d) {
			step++
			continue
		}
		prompt, def := st.ask(d)
		fmt.Fprint(out, prompt)
		if def != "" {
			fmt.Fprintf(out, " [%s]", cleanText(def))
		}
		fmt.Fprint(out, ": ")
		value, err := readPlainAnswer(r, st.secret, secret)
		if err != nil {
			return providerWizardResult{}, err
		}
		switch strings.ToLower(value) {
		case "cancel":
			return providerWizardResult{}, errProviderWizardCanceled
		case "back":
			step = previousPlainStep(d, step)
			continue
		}
		if value == "" && !st.blank {
			value = def
		}
		if err := st.set(d, value); err != nil {
			fmt.Fprintln(out, err)
			continue
		}
		step++
	}
	return d.result(), nil
}

// readPlainAnswer reads one trimmed line, or the hidden secret; end of input cancels setup.
func readPlainAnswer(r *bufio.Reader, hidden bool, secret func() (string, error)) (string, error) {
	var value string
	var err error
	if hidden && secret != nil {
		value, err = secret()
	} else {
		value, err = r.ReadString('\n')
	}
	if errors.Is(err, io.EOF) {
		return "", errProviderWizardCanceled
	}
	return strings.TrimSpace(value), err
}

func previousPlainStep(d *providerWizardDraft, step int) int {
	if step == 0 {
		return 0
	}
	step--
	for step > 0 && plainSteps[step].skip(d) {
		step--
	}
	return step
}

func wizardYesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
