package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

const (
	accent = "36"
	dim    = "2"
	bold   = "1"
	red    = "31"
	yellow = "33"
	green  = "32"
)

func colorEnabled(tty bool) bool {
	return tty && os.Getenv("TERM") != "dumb" && os.Getenv("NO_COLOR") == ""
}

func terminal(f *os.File) bool {
	return os.Getenv("TERM") != "dumb" && term.IsTerminal(int(f.Fd()))
}

func paint(text, code string, enabled bool) string {
	if !enabled {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func fitText(s string, width int) string {
	return runewidth.Truncate(cleanText(s), max(width, 0), "…")
}

func padText(s string, width int) string {
	s = cleanText(s)
	return s + strings.Repeat(" ", max(0, width-runewidth.StringWidth(s)))
}

func severityColor(severity string) string {
	switch severity {
	case "error":
		return red
	case "warning":
		return yellow
	default:
		return accent
	}
}

func renderModelChoices(w io.Writer, choices []modelChoice, active, activeModel string, color bool) {
	fmt.Fprintf(w, "\n  %s\n", paint("CHOOSE A MODEL", bold, color))
	width := 8
	for _, choice := range choices {
		width = max(width, runewidth.StringWidth(cleanText(choice.provider)))
	}
	for i, choice := range choices {
		selected := choice.provider == active && choice.model == activeModel
		mark, suffix := " ", ""
		if selected {
			mark, suffix = "*", "  active"
		}
		fmt.Fprintf(w, "  %s %2d  %s  %s%s\n", paint(mark, accent, color), i+1,
			padText(choice.provider, width), cleanText(choice.model), paint(suffix, dim, color))
	}
	fmt.Fprintln(w)
}

func renderFindings(w io.Writer, findings []Finding, color bool) {
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(w, "\n  %s\n", paint("FINDINGS", bold, color))
	for _, f := range findings {
		severity := padText(strings.ToUpper(f.Severity), 7)
		location := cleanText(fmt.Sprintf("%s:%d", f.File, f.Line))
		fmt.Fprintf(w, "\n  %s  %s  %s\n", paint(severity, severityColor(f.Severity), color), paint(location, bold, color), paint(cleanText(f.Rule), dim, color))
		fmt.Fprintf(w, "        %s  %s\n", cleanText(f.Message), paint(fmt.Sprintf("(%.2f confidence)", f.Confidence), dim, color))
		if f.Quote != "" {
			fmt.Fprintf(w, "        %s\n", paint("│ "+cleanText(f.Quote), dim, color))
		}
	}
	fmt.Fprintln(w)
}
