package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/mattn/go-runewidth"
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
	if len(c.Providers) == 0 {
		fmt.Println("no providers configured; run: lintdec provider add")
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
		fmt.Printf("\n  %s\n\n", paint("MODELS", bold, colorEnabled(true)))
	}
	picked := 0
	for _, name := range names {
		p := c.Providers[name]
		if len(p.Models) == 0 {
			fmt.Printf("  %s (no models yet, run: lintdec model %s)\n", padText(name, width), name)
			continue
		}
		for _, m := range p.Models {
			mark := " "
			if name == c.Active && m == c.activeModelFor(name, p) {
				mark = "*"
			}
			fmt.Printf("%s %s %s\n", paint(mark, accent, colorEnabled(interactive)), padText(name, width), cleanText(m))
			picked++
		}
	}
	if picked == 0 {
		fmt.Println("pick a model: lintdec model <provider>")
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

func fetchChoicesForProvider(name string, p Provider, interactive bool) []modelChoice {
	ids, err := fetchModels(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: %s /models: %v (using saved models)\n", name, err)
		ids = p.Models
	}
	if !interactive && len(ids) > 20 {
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

func modelPickerForm(choices []modelChoice, selected *modelChoice) *huh.Form {
	options := make([]huh.Option[modelChoice], 0, len(choices))
	for _, choice := range choices {
		options = append(options, huh.NewOption(cleanText(choice.provider)+"  "+cleanText(choice.model), choice))
	}
	keys := huh.NewDefaultKeyMap()
	keys.Quit.SetKeys("ctrl+c", "esc")
	return huh.NewForm(huh.NewGroup(huh.NewSelect[modelChoice]().
		Title("Choose a model").Description("↑/↓ to move, / to filter, enter to select, esc or ctrl+c to cancel.").
		Height(min(12, len(choices)+2)).Value(selected).Options(options...))).WithKeyMap(keys).WithShowHelp(true)
}

func runModelPicker(choices []modelChoice, active modelChoice, in io.Reader, out io.Writer) (modelChoice, error) {
	selected := active
	form := modelPickerForm(choices, &selected)
	if !colorEnabled(true) {
		form.WithProgramOptions(tea.WithColorProfile(colorprofile.Ascii))
	}
	form.WithInput(in).WithOutput(out)
	if err := form.Run(); err != nil {
		return modelChoice{}, err
	}
	if form.State != huh.StateCompleted {
		return modelChoice{}, huh.ErrUserAborted
	}
	return selected, nil
}

func modelPick(c *Config, rest []string) int {
	if len(c.Providers) == 0 {
		fmt.Fprintln(os.Stderr, "no providers configured; run: lintdec provider add")
		return 2
	}
	interactive := os.Getenv("LINTDEC_ACCESSIBLE") != "1" && terminal(os.Stdin) && terminal(os.Stdout)
	choices, err := modelChoices(c, rest, interactive)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	activeModel := ""
	if p, ok := c.Providers[c.Active]; ok {
		activeModel = c.activeModelFor(c.Active, p)
	}
	if interactive {
		choice, err := runModelPicker(choices, modelChoice{provider: c.Active, model: activeModel}, os.Stdin, os.Stdout)
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Fprintln(os.Stderr, "model selection canceled")
			return 130
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return selectModel(c, choice.provider, choice.model)
	}
	return pickModelByNumber(c, choices, activeModel)
}

// modelChoices: the named provider's models, else saved models, else the active provider's catalog.
func modelChoices(c *Config, rest []string, interactive bool) ([]modelChoice, error) {
	var choices []modelChoice
	if len(rest) > 0 {
		p, ok := c.Providers[rest[0]]
		if !ok {
			return nil, fmt.Errorf("no such provider: %s", rest[0])
		}
		choices = fetchChoicesForProvider(rest[0], p, interactive)
	} else if choices = savedModelChoices(*c); len(choices) == 0 && c.Active != "" {
		choices = fetchChoicesForProvider(c.Active, c.Providers[c.Active], interactive)
	}
	if len(choices) == 0 {
		return nil, errors.New("no models found; add one with: lintdec model add <provider> <model>")
	}
	return choices, nil
}

// pickModelByNumber is the plain prompt: a numbered list with the active model as the default.
func pickModelByNumber(c *Config, choices []modelChoice, activeModel string) int {
	isActive := func(ch modelChoice) bool { return ch.provider == c.Active && ch.model == activeModel }
	if terminal(os.Stdout) {
		renderModelChoices(os.Stdout, choices, c.Active, activeModel, colorEnabled(true))
	} else {
		for i, ch := range choices {
			mark := " "
			if isActive(ch) {
				mark = "*"
			}
			fmt.Printf("%s %2d) %-12s %s\n", mark, i+1, ch.provider, ch.model)
		}
	}
	defaultChoice := ""
	if i := slices.IndexFunc(choices, isActive); i >= 0 {
		defaultChoice = strconv.Itoa(i + 1)
	}
	var raw string
	ask(&raw, "pick number", defaultChoice)
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
