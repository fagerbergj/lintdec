package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectConfig is the lintdec.yaml subset: flat scalars plus block lists of
// strings or "- key: value" pairs. A hand-rolled parser keeps lintdec dep-free.
type ProjectConfig struct {
	Path     string
	Severity string
	RulesOff []string
	RulesOn  []string
	Include  []string
	Exclude  []string
	Model    string
	Provider string
}

func loadProjectConfig(dir string) (ProjectConfig, []string) {
	var pc ProjectConfig
	var warn []string
	d := dir
	for {
		p := filepath.Join(d, "lintdec.yaml")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			pc.Path = p
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			return pc, warn
		}
		d = parent
	}
	b, err := os.ReadFile(pc.Path)
	if err != nil {
		warn = append(warn, fmt.Sprintf("%s: %v", pc.Path, err))
		return pc, warn
	}
	key := ""
	for i, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			k, v, _ := strings.Cut(t, ":")
			key = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			switch key {
			case "severity":
				pc.Severity = v
			case "model":
				pc.Model = v
			case "provider":
				pc.Provider = v
			case "rules", "include", "exclude":
			case "":
			default:
				warn = append(warn, fmt.Sprintf("%s:%d: unknown key %q (ignored)", pc.Path, i+1, key))
			}
		} else {
			item := strings.TrimSpace(strings.TrimPrefix(t, "-"))
			k, v, hasColon := strings.Cut(item, ":")
			switch key {
			case "rules":
				if hasColon {
					switch strings.TrimSpace(k) {
					case "off":
						pc.RulesOff = append(pc.RulesOff, strings.Trim(strings.TrimSpace(v), `"'`))
					case "on":
						pc.RulesOn = append(pc.RulesOn, strings.Trim(strings.TrimSpace(v), `"'`))
					default:
						warn = append(warn, fmt.Sprintf("%s:%d: expected '- off: id' or '- on: id' in rules", pc.Path, i+1))
					}
				} else {
					pc.RulesOff = append(pc.RulesOff, strings.Trim(item, `"'`))
				}
			case "include":
				pc.Include = append(pc.Include, strings.Trim(item, `"'`))
			case "exclude":
				pc.Exclude = append(pc.Exclude, strings.Trim(item, `"'`))
			}
		}
	}
	return pc, warn
}
