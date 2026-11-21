package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type Finding struct {
	Rule       string  `json:"rule"`
	Severity   string  `json:"severity"`
	Confidence float64 `json:"confidence"`
	File       string  `json:"file"`
	Line       int     `json:"line"`
	Quote      string  `json:"quote,omitempty"`
	Message    string  `json:"message"`
}

func severityRank(s string) int {
	switch s {
	case "high":
		return 2
	case "med":
		return 1
	default:
		return 0
	}
}

// sortFindings orders by severity, then confidence (most certain first),
// then file/line so the report reads like a worklist.
func sortFindings(fs []Finding) {
	sort.Slice(fs, func(i, j int) bool {
		if severityRank(fs[i].Severity) != severityRank(fs[j].Severity) {
			return severityRank(fs[i].Severity) > severityRank(fs[j].Severity)
		}
		if fs[i].Confidence != fs[j].Confidence {
			return fs[i].Confidence > fs[j].Confidence
		}
		if fs[i].File != fs[j].File {
			return fs[i].File < fs[j].File
		}
		if fs[i].Line != fs[j].Line {
			return fs[i].Line < fs[j].Line
		}
		return fs[i].Rule < fs[j].Rule
	})
}

func emit(findings []Finding, out string) {
	switch out {
	case "jsonl":
		enc := json.NewEncoder(os.Stdout)
		for _, f := range findings {
			enc.Encode(f)
		}
	case "json":
		b, _ := json.MarshalIndent(findings, "", "  ")
		fmt.Println(string(b))
	default:
		for _, f := range findings {
			fmt.Printf("%s:%d  %-22s [%-4s %.2f] %s\n", f.File, f.Line, f.Rule, f.Severity, f.Confidence, f.Message)
			if f.Quote != "" {
				q := strings.ReplaceAll(f.Quote, "\n", " ")
				if len(q) > 100 {
					q = q[:100] + "…"
				}
				fmt.Printf("        | %s\n", q)
			}
		}
	}
}

func formatDuration(sec float64) string {
	if sec <= 0 {
		return "—"
	}
	if sec < 60 {
		return fmt.Sprintf("%.0fs", sec)
	}
	m := int(sec / 60)
	return fmt.Sprintf("%dm%02ds", m, int(sec)%60)
}

func formatRate(perSec float64) string {
	if perSec <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f/min", perSec*60)
}
