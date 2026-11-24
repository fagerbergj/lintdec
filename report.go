package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

type Finding struct {
	Rule       string   `json:"rule"`
	Severity   string   `json:"severity"`
	Confidence float64  `json:"confidence"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	EndLine    int      `json:"end_line,omitempty"` // last line of the checked unit
	Span       string   `json:"span,omitempty"`     // the exact words judged, for word-level rules
	Quote      string   `json:"quote,omitempty"`
	Message    string   `json:"message"`
	Veto       *float64 `json:"veto,omitempty"`
	Source     string   `json:"source,omitempty"` // the published rule this finding enforces
}

func severityRank(s string) int {
	switch s {
	case "error":
		return 2
	case "warning":
		return 1
	case "info":
		return 0
	default:
		return -1
	}
}

// oncePerFile: rules whose hits in one file share one fix, so they read as one task with a count.
var oncePerFile = map[string]bool{"md-wordy-phrase": true}

func collapsePerFile(fs []Finding) []Finding {
	type key struct{ rule, file string }
	if fs == nil {
		return nil
	}
	at, count := map[key]int{}, map[key]int{}
	out := make([]Finding, 0, len(fs)) // keep an empty, non-nil report as [] in JSON
	for _, f := range fs {
		if !oncePerFile[f.Rule] {
			out = append(out, f)
			continue
		}
		k := key{f.Rule, f.File}
		count[k]++
		if i, ok := at[k]; ok {
			if f.Line < out[i].Line {
				out[i] = f
			}
			continue
		}
		at[k] = len(out)
		out = append(out, f)
	}
	for k, i := range at {
		if n := count[k]; n > 1 {
			out[i].Message += fmt.Sprintf(" (%d places in this file)", n)
		}
	}
	return out
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
		if terminal(os.Stdout) {
			renderFindings(os.Stdout, findings, colorEnabled(true))
			return
		}
		for _, f := range findings {
			fmt.Printf("%s:%d  %-22s [%-7s %.2f] %s\n", f.File, f.Line, f.Rule, f.Severity, f.Confidence, f.Message)
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

type reportFile interface {
	io.WriteCloser
	Name() string
	Chmod(os.FileMode) error
}

func reportText(s string) string {
	var text strings.Builder
	for _, r := range ansi.Strip(s) {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			quoted := strconv.QuoteRune(r)
			text.WriteString(quoted[1 : len(quoted)-1])
		} else {
			text.WriteRune(r)
		}
	}
	return text.String()
}

func reportLocation(file string, line int) string {
	if strings.IndexFunc(file, unicode.IsControl) >= 0 {
		file = strconv.Quote(file)
	}
	return fmt.Sprintf("%s:%d", file, line)
}

func writeReportDetail(w io.Writer, text, prefix string, width int, color bool) error {
	text = reportText(text)
	if width > 0 {
		text = strings.ReplaceAll(text, "\t", "\\t")
		if width <= runewidth.StringWidth(prefix)+1 {
			prefix = strings.TrimSpace(prefix) + " "
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if width > 0 {
			line = ansi.Hardwrap(line, max(width-runewidth.StringWidth(prefix), 1), true)
		}
		for _, part := range strings.Split(line, "\n") {
			if _, err := fmt.Fprintf(w, "%s%s\n", prefix, paint(part, dim, color)); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeFindingReport(w io.Writer, findings []Finding, color bool, width int) error {
	if len(findings) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n  %s\n", paint("FINDINGS", bold, color)); err != nil {
		return err
	}
	for _, f := range findings {
		severity := paint(cleanText(ansi.Strip(strings.ToUpper(f.Severity))), severityColor(f.Severity), color)
		location := paint(reportLocation(f.File, f.Line), bold, color)
		rule := paint(cleanText(ansi.Strip(f.Rule)), dim, color)
		confidence := paint(fmt.Sprintf("(%g confidence)", f.Confidence), dim, color)
		header := fmt.Sprintf("%s  %s  %s  %s", severity, location, rule, confidence)
		if width > 0 && runewidth.StringWidth(ansi.Strip(header))+2 > width {
			metadata := ansi.Hardwrap(fmt.Sprintf("%s  %s  %s", severity, rule, confidence), max(width-2, 1), true)
			if _, err := fmt.Fprintf(w, "\n  %s\n  %s\n", location, strings.ReplaceAll(metadata, "\n", "\n  ")); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(w, "\n  %s\n", header); err != nil {
			return err
		}
		if err := writeReportDetail(w, f.Message, "        · ", width, color); err != nil {
			return err
		}
		if f.Quote != "" {
			if err := writeReportDetail(w, f.Quote, "        │ ", width, color); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

const findingPreviewLimit = 5

func saveFindingReport(warnings io.Writer, findings []Finding, createTemp func() (reportFile, error)) (string, error) {
	file, err := createTemp()
	if err != nil {
		return "", err
	}
	// A restrictive umask can remove the owner's read permission.
	err = file.Chmod(0o600)
	if err == nil {
		err = writeFindingReport(file, findings, false, 0)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if removeErr := os.Remove(file.Name()); removeErr != nil {
			fmt.Fprintf(warnings, "warn: could not remove incomplete private report %q\n", file.Name())
		}
		return "", err
	}
	return file.Name(), nil
}

func interactiveResults(out string, prog *progress) bool {
	if out == "json" || out == "jsonl" || prog.quiet || !prog.tty || os.Getenv("CI") != "" || !terminal(os.Stdout) || !terminal(os.Stderr) {
		return false
	}
	stdout, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	stderr, err := os.Stderr.Stat()
	return err == nil && os.SameFile(stdout, stderr)
}

func terminalWidth(f *os.File) int {
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width < 1 {
		return 80
	}
	return width
}

func renderFindingTable(w io.Writer, findings []Finding, width int, color bool) {
	if len(findings) == 0 {
		return
	}
	indent := "  "
	if width < 4 {
		indent = ""
	}
	printLine := func(line string) {
		wrapped := ansi.Hardwrap(line, max(width-len(indent), 1), true)
		fmt.Fprintln(w, indent+strings.ReplaceAll(wrapped, "\n", "\n"+indent))
	}
	shown := min(len(findings), findingPreviewLimit)
	printLine(paint(fmt.Sprintf("FINDINGS · %d of %d", shown, len(findings)), bold, color))
	headers := []string{"Level", "Confidence", "Location", "Rule"}
	rows := make([][]string, 0, shown)
	for _, f := range findings[:shown] {
		level := cleanText(ansi.Strip(strings.ToUpper(f.Severity)))
		rows = append(rows, []string{
			paint(level, severityColor(f.Severity), color),
			fmt.Sprintf("%.2f", f.Confidence),
			reportLocation(f.File, f.Line),
			cleanText(ansi.Strip(f.Rule)),
		})
	}

	if width >= 40 {
		t := table.New().Headers(headers...).StyleFunc(func(_, _ int) lipgloss.Style {
			return lipgloss.NewStyle().Padding(0, 1)
		})
		for _, row := range rows {
			t.Row(row[0], row[1], row[2], ansi.Hardwrap(row[3], 24, true))
		}
		rendered := t.String()
		fits := tableFits(rendered, headers, rows, width-len(indent))
		if fits {
			fmt.Fprintln(w, indent+strings.ReplaceAll(rendered, "\n", "\n"+indent))
			return
		}
	}
	printLine(strings.Join(headers, "  "))
	for _, row := range rows {
		joined := strings.Join(row, "  ")
		if runewidth.StringWidth(ansi.Strip(joined)) <= width-len(indent) {
			printLine(joined)
			continue
		}
		printLine(strings.Join(row[:2], "  "))
		fmt.Fprintf(w, "%s%s\n", indent, row[2])
		printLine(row[3])
	}
}

// tableFits: whether the rendered table kept its borders, headers, and locations within width.
// Table width is best-effort and can clip headers or borders.
func tableFits(rendered string, headers []string, rows [][]string, width int) bool {
	lines := strings.Split(ansi.Strip(rendered), "\n")
	fits := len(lines) > 2 && strings.HasSuffix(lines[0], "┐") && strings.HasSuffix(lines[len(lines)-1], "┘")
	for _, header := range headers {
		fits = fits && strings.Contains(lines[1], header)
	}
	for _, row := range rows {
		fits = fits && strings.Contains(ansi.Strip(rendered), row[2])
	}
	for _, line := range lines {
		bordered := strings.HasSuffix(line, "┐") || strings.HasSuffix(line, "┤") || strings.HasSuffix(line, "│") || strings.HasSuffix(line, "┘")
		fits = fits && runewidth.StringWidth(line) <= width && bordered
	}
	return fits
}

func emitResults(out, warnings io.Writer, findings []Finding, color bool, createTemp func() (reportFile, error)) {
	if len(findings) > findingPreviewLimit {
		path, err := saveFindingReport(warnings, findings, createTemp)
		if err == nil {
			var absolute string
			absolute, err = filepath.Abs(path)
			if err == nil {
				renderFindingTable(out, findings, terminalWidth(os.Stdout), color)
				if strings.IndexFunc(absolute, unicode.IsControl) >= 0 {
					absolute = strconv.Quote(absolute)
				}
				fmt.Fprintf(out, "  Full report: %s\n", absolute)
				return
			}
			if removeErr := os.Remove(path); removeErr != nil {
				fmt.Fprintf(warnings, "warn: could not remove incomplete private report %q\n", path)
			}
		}
		width := terminalWidth(os.Stdout)
		warning := ansi.Hardwrap("warn: could not save private report; showing all findings", max(width-2, 1), true)
		fmt.Fprintln(warnings, strings.ReplaceAll(warning, "\n", "\n  "))
		writeFindingReport(out, findings, color, width)
		return
	}
	width := terminalWidth(os.Stdout)
	renderFindingTable(out, findings, width, color)
	for _, f := range findings {
		fmt.Fprintf(out, "  %s\n", reportLocation(f.File, f.Line))
		writeReportDetail(out, f.Message, "    · ", width, color)
		if f.Quote != "" {
			writeReportDetail(out, f.Quote, "    │ ", width, color)
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
