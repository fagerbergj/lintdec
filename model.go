package main

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

type modelChoice struct {
	provider string
	model    string
}

func modelCmd(args []string) int {
	c, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if len(args) == 0 {
		return modelPick(&c, nil)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return modelList(c)
	case "use":
		return modelUse(&c, rest)
	case "add":
		return modelAdd(&c, rest)
	case "rm":
		return modelRm(&c, rest)
	default:
		if _, ok := c.Providers[sub]; ok {
			return modelPick(&c, []string{sub})
		}
		fmt.Fprintf(os.Stderr, "unknown subcommand or provider: %s (list|use|add|rm|<provider>)\n", sub)
		return 2
	}
}

func modelList(c Config) int {
	choices := savedModelChoices(c)
	if len(choices) == 0 {
		fmt.Println("no models configured; run: lintdec model")
		return 0
	}
	for _, ch := range choices {
		mark := " "
		if ch.provider == c.Active && ch.model == c.activeModelFor(ch.provider, c.Providers[ch.provider]) {
			mark = "*"
		}
		fmt.Printf("%s %-12s %s\n", mark, ch.provider, ch.model)
	}
	return 0
}

func savedModelChoices(c Config) []modelChoice {
	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	slices.Sort(names)
	var out []modelChoice
	for _, name := range names {
		for _, m := range c.Providers[name].Models {
			out = append(out, modelChoice{provider: name, model: m})
		}
	}
	return out
}

func fetchChoicesForProvider(name string, p Provider) []modelChoice {
	ids, err := fetchModels(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: %s /models: %v (using saved models)\n", name, err)
		ids = p.Models
	}
	if len(ids) > 20 {
		var filter string
		ask(&filter, fmt.Sprintf("%d models on %s — filter substring (blank for all)", len(ids), name), "")
		if filter != "" {
			lo := strings.ToLower(filter)
			ids = slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
				return !strings.Contains(strings.ToLower(id), lo)
			})
		}
	}
	out := make([]modelChoice, 0, len(ids))
	for _, id := range ids {
		out = append(out, modelChoice{provider: name, model: id})
	}
	return out
}

func modelPick(c *Config, rest []string) int {
	if len(c.Providers) == 0 {
		fmt.Fprintln(os.Stderr, "no providers configured; run: lintdec provider add")
		return 2
	}
	var choices []modelChoice
	if len(rest) > 0 {
		p, ok := c.Providers[rest[0]]
		if !ok {
			fmt.Fprintf(os.Stderr, "no such provider: %s\n", rest[0])
			return 2
		}
		choices = fetchChoicesForProvider(rest[0], p)
	} else {
		choices = savedModelChoices(*c)
		if len(choices) == 0 && c.Active != "" {
			choices = fetchChoicesForProvider(c.Active, c.Providers[c.Active])
		}
	}
	if len(choices) == 0 {
		fmt.Fprintln(os.Stderr, "no models found; add one with: lintdec model add <provider> <model>")
		return 2
	}
	for i, ch := range choices {
		mark := " "
		if ch.provider == c.Active && ch.model == c.activeModelFor(ch.provider, c.Providers[ch.provider]) {
			mark = "*"
		}
		fmt.Printf("%s %2d) %-12s %s\n", mark, i+1, ch.provider, ch.model)
	}
	var raw string
	ask(&raw, "pick number", "")
	idx, err := strconv.Atoi(raw)
	if err != nil || idx < 1 || idx > len(choices) {
		fmt.Fprintf(os.Stderr, "invalid selection: %q\n", raw)
		return 2
	}
	return selectModel(c, choices[idx-1].provider, choices[idx-1].model)
}

func modelUse(c *Config, rest []string) int {
	if len(rest) == 0 {
		return modelPick(c, nil)
	}
	if len(rest) >= 2 {
		return selectModel(c, rest[0], rest[1])
	}
	name, _, ok := c.findProviderForModel(rest[0])
	if !ok {
		if c.Active == "" {
			fmt.Fprintf(os.Stderr, "model %q not found on a unique provider; pass: lintdec model use <provider> <model>\n", rest[0])
			return 2
		}
		name = c.Active
	}
	return selectModel(c, name, rest[0])
}

func selectModel(c *Config, provName, model string) int {
	p, ok := c.Providers[provName]
	if !ok {
		fmt.Fprintf(os.Stderr, "no such provider: %s\n", provName)
		return 2
	}
	if !slices.Contains(p.Models, model) {
		p.Models = append(p.Models, model)
		c.Providers[provName] = p
	}
	c.Active = provName
	c.ActiveModel = model
	if err := c.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Printf("active model: %s (%s)\n", model, provName)
	return 0
}

func modelAdd(c *Config, rest []string) int {
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lintdec model add <provider> <model>...")
		return 2
	}
	name, models := rest[0], rest[1:]
	p, ok := c.Providers[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "no such provider: %s\n", name)
		return 2
	}
	for _, m := range models {
		if !slices.Contains(p.Models, m) {
			p.Models = append(p.Models, m)
		}
	}
	if c.Active == name && c.ActiveModel == "" {
		c.ActiveModel = p.defaultModel()
	}
	c.Providers[name] = p
	if err := c.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Printf("%s models: %s\n", name, strings.Join(p.Models, ", "))
	return 0
}

func modelRm(c *Config, rest []string) int {
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lintdec model rm <provider> <model>...")
		return 2
	}
	name, models := rest[0], rest[1:]
	p, ok := c.Providers[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "no such provider: %s\n", name)
		return 2
	}
	p.Models = slices.DeleteFunc(p.Models, func(m string) bool { return slices.Contains(models, m) })
	if c.Active == name && slices.Contains(models, c.ActiveModel) {
		c.ActiveModel = p.defaultModel()
	}
	c.Providers[name] = p
	if err := c.save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Printf("%s models: %s\n", name, strings.Join(p.Models, ", "))
	return 0
}
