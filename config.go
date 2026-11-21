package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Provider struct {
	BaseURL string   `json:"baseUrl"`
	APIKey  string   `json:"apiKey,omitempty"`
	Models  []string `json:"models,omitempty"`
	Model   string   `json:"model,omitempty"` // legacy; migrated into Models on load
}

func (p Provider) defaultModel() string {
	if len(p.Models) > 0 {
		return p.Models[0]
	}
	return p.Model
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
	for name, p := range c.Providers {
		if p.Model != "" {
			if !slices.Contains(p.Models, p.Model) {
				p.Models = append([]string{p.Model}, p.Models...)
			}
			p.Model = ""
			c.Providers[name] = p
		}
	}
	return c, nil
}

func (c Config) save() error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

var stdinReader = bufio.NewReader(os.Stdin)

func ask(v *string, label, def string) string {
	if def != "" {
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

func fetchModels(p Provider) ([]string, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	base := strings.TrimRight(p.BaseURL, "/")
	req, err := http.NewRequest("GET", base+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var m struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(m.Data))
	for _, d := range m.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func testConnection(p Provider, model string) error {
	if model == "" {
		model = p.defaultModel()
	}
	if ids, err := fetchModels(p); err == nil && len(ids) > 0 {
		if model == "" {
			fmt.Printf("connected; %d models available\n", len(ids))
		} else if !slices.Contains(ids, model) {
			fmt.Printf("connected; %d models available — note: %q is not listed\n", len(ids), model)
		} else {
			fmt.Printf("connected; %s is available (%d models total)\n", model, len(ids))
		}
		return nil
	} else if err != nil && model == "" {
		return err
	}
	// /models missing or empty: try a 1-token completion
	c := &http.Client{Timeout: 60 * time.Second}
	base := strings.TrimRight(p.BaseURL, "/")
	payload, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 5, "temperature": 0,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	})
	req2, err := http.NewRequest("POST", base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req2.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req2.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	res2, err := c.Do(req2)
	if err != nil {
		return err
	}
	b2, _ := io.ReadAll(io.LimitReader(res2.Body, 4096))
	res2.Body.Close()
	if res2.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", res2.StatusCode, strings.TrimSpace(string(b2)))
	}
	fmt.Printf("connected; completion succeeded (%s)\n", model)
	return nil
}

func providerAdd(c *Config, rest []string) int {
	defName := "local"
	if len(rest) > 0 {
		defName = rest[0]
	}
	var name string
	ask(&name, "name", defName)
	if _, exists := c.Providers[name]; exists {
		fmt.Fprintf(os.Stderr, "provider %q already exists\n", name)
		return 2
	}
	var p Provider
	p.BaseURL = strings.TrimRight(ask(&p.BaseURL, "base URL", "http://localhost:11434/v1"), "/")
	ask(&p.APIKey, "api key (blank if none)", "")
	var yes string
	if strings.HasPrefix(strings.ToLower(ask(&yes, "test the connection now?", "y")), "y") {
		if err := testConnection(p, ""); err != nil {
			fmt.Fprintf(os.Stderr, "connection test failed: %v\n", err)
			var k string
			if !strings.HasPrefix(strings.ToLower(ask(&k, "add it anyway?", "n")), "y") {
				return 2
			}
		}
	}
	c.Providers[name] = p
	var act string
	if strings.HasPrefix(strings.ToLower(ask(&act, "set as active?", "y")), "y") || c.Active == "" {
		c.Active = name
		c.ActiveModel = ""
	}
	if err := c.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	suffix := ""
	if c.Active == name {
		suffix = " (active)"
	}
	fmt.Printf("added %s%s — run 'lintdec model pick %s' to choose a model\n", name, suffix, name)
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
	for _, name := range names {
		p := c.Providers[name]
		mark := " "
		if name == c.Active {
			mark = "*"
		}
		fmt.Printf("%s %-12s %s\n", mark, name, p.BaseURL)
		activeModel := c.activeModelFor(name, p)
		for _, m := range p.Models {
			mMark := " "
			if name == c.Active && m == activeModel {
				mMark = "*"
			}
			fmt.Printf("    %s %s\n", mMark, m)
		}
	}
	return 0
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

func providerRm(c *Config, rest []string) int {
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: lintdec provider rm <name>")
		return 2
	}
	if _, ok := c.Providers[rest[0]]; !ok {
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
	fmt.Printf("removed %s\n", rest[0])
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
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s (list|add|use|rm|test)\n", sub)
		return 2
	}
}
