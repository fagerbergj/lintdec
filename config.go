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
	BaseURL string `json:"baseUrl"`
	Model   string `json:"model"`
	APIKey  string `json:"apiKey,omitempty"`
}

type Config struct {
	Active    string              `json:"active"`
	Providers map[string]Provider `json:"providers"`
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

func testConnection(p Provider) error {
	c := &http.Client{Timeout: 60 * time.Second}
	base := strings.TrimRight(p.BaseURL, "/")
	req, err := http.NewRequest("GET", base+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	res.Body.Close()
	if res.StatusCode < 400 {
		var m struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &m) == nil && len(m.Data) > 0 {
			ids := make([]string, 0, len(m.Data))
			for _, d := range m.Data {
				ids = append(ids, d.ID)
			}
			if !slices.Contains(ids, p.Model) {
				fmt.Printf("connected; %d models (%s) — note: %q is not listed\n", len(ids), strings.Join(ids, ", "), p.Model)
			} else {
				fmt.Printf("connected; %s is available (%d models total)\n", p.Model, len(ids))
			}
			return nil
		}
	}
	// /models missing or empty: try a 1-token completion
	payload, _ := json.Marshal(map[string]any{
		"model": p.Model, "max_tokens": 5, "temperature": 0,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	})
	req2, err := http.NewRequest("POST", base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+p.APIKey)
	res2, err := c.Do(req2)
	if err != nil {
		return err
	}
	b2, _ := io.ReadAll(io.LimitReader(res2.Body, 4096))
	res2.Body.Close()
	if res2.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", res2.StatusCode, strings.TrimSpace(string(b2)))
	}
	fmt.Printf("connected; completion succeeded\n")
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
	ask(&p.Model, "model id", "")
	if p.Model == "" {
		fmt.Fprintln(os.Stderr, "model id is required")
		return 2
	}
	ask(&p.APIKey, "api key (blank if none)", "")
	var yes string
	if strings.HasPrefix(strings.ToLower(ask(&yes, "test the connection now?", "y")), "y") {
		if err := testConnection(p); err != nil {
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
	}
	if err := c.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	suffix := ""
	if c.Active == name {
		suffix = " (active)"
	}
	fmt.Printf("added %s%s\n", name, suffix)
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
		fmt.Printf("%s %-12s %-32s model %s\n", mark, name, p.BaseURL, p.Model)
	}
	return 0
}

func providerUse(c *Config, rest []string) int {
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: lintdec provider use <name>")
		return 2
	}
	if _, ok := c.Providers[rest[0]]; !ok {
		fmt.Fprintf(os.Stderr, "no such provider: %s\n", rest[0])
		return 2
	}
	c.Active = rest[0]
	if err := c.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Printf("active provider: %s\n", c.Active)
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
	if err := testConnection(p); err != nil {
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
