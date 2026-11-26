package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

type progress struct {
	mu        sync.Mutex
	tty       bool
	quiet     bool
	total     int
	done      int
	inflight  int
	cached    int
	skipped   int
	start     time.Time
	calls     int
	last      string
	active    map[string]int
	ledger    []Finding
	ledgerN   int
	bySev     map[string]int
	milestone int
	panelH    int
	costs     *costAccumulator
}

func newProgress(total int, quiet bool) *progress {
	return &progress{tty: terminal(os.Stderr), quiet: quiet, total: total, start: time.Now(), bySev: map[string]int{}}
}

func (p *progress) tick(d time.Duration, cached bool, last string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	if p.inflight > 0 {
		p.inflight--
	}
	if cached {
		p.cached++
	}
	if d > 0 && !cached {
		p.calls++
	}
	p.last = last
	if p.quiet {
		return
	}
	if !p.tty {
		thresholds := [4]int{25, 50, 75, 100}
		if p.total > 0 && p.milestone < len(thresholds) && p.done*100/p.total >= thresholds[p.milestone] {
			p.milestone++
			fmt.Fprintf(os.Stderr, "[%3d%%] %d/%d · %s · eta %s · %d cached\n",
				thresholds[p.milestone-1], p.done, p.total, p.rate(), p.eta(), p.cached)
		}
		return
	}
	p.drawBar()
}

func (p *progress) addFinding(f Finding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ledger = append(p.ledger, f)
	p.ledgerN++
	p.bySev[f.Severity]++
	if len(p.ledger) > 8 {
		p.ledger = p.ledger[len(p.ledger)-8:]
	}
	if p.tty && !p.quiet {
		p.drawBar()
	}
}

func (p *progress) noteSubmit() {
	p.mu.Lock()
	p.inflight++
	p.mu.Unlock()
}

func (p *progress) beginFile(file string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		p.active = make(map[string]int)
	}
	p.active[file]++
	if p.tty && !p.quiet {
		p.drawBar()
	}
}

func (p *progress) endFile(file string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[file]--
	if p.active[file] == 0 {
		delete(p.active, file)
	}
	if p.tty && !p.quiet {
		p.drawBar()
	}
}

func (p *progress) checksPerSec() float64 {
	if p.calls == 0 {
		return 0
	}
	elapsed := time.Since(p.start).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(p.calls) / elapsed
}

func (p *progress) rate() string {
	r := p.checksPerSec()
	if r <= 0 {
		return "—"
	}
	return formatRate(r)
}

func (p *progress) eta() string {
	r := p.checksPerSec()
	if r <= 0 {
		return "—"
	}
	return formatDuration(float64(max(p.total-p.done, 0)) / r)
}

func (p *progress) lines(width int, color bool) []string {
	width = max(width, 1)
	frac := 0.0
	if p.total > 0 {
		frac = min(float64(p.done)/float64(p.total), 1)
	}
	barWidth := max(4, min(28, width-24))
	filled := int(frac*float64(barWidth) + 0.5)
	bar := strings.Repeat("━", filled) + strings.Repeat("─", barWidth-filled)
	lines := []string{
		paint(fitText(fmt.Sprintf("  %s  %3.0f%%  %d/%d checks", bar, frac*100, p.done, p.total), width), accent, color),
		paint(fitText(fmt.Sprintf("  %s  ·  eta %s  ·  %d cached  ·  %d pending", p.rate(), p.eta(), p.cached, p.inflight), width), dim, color),
	}
	files := make([]string, 0, len(p.active))
	for file := range p.active {
		files = append(files, file)
	}
	slices.Sort(files)
	for _, file := range files[:min(2, len(files))] {
		lines = append(lines, fitText("  checking  "+file, width))
	}
	if len(files) > 2 {
		lines = append(lines, paint(fitText(fmt.Sprintf("  +%d other files", len(files)-2), width), dim, color))
	} else if len(files) == 0 && p.last != "" {
		lines = append(lines, fitText("  latest  "+p.last, width))
	}
	if p.ledgerN > 0 {
		lines = append(lines, paint(fitText(fmt.Sprintf("  findings  %d  ·  %d error  %d warning  %d info", p.ledgerN, p.bySev["error"], p.bySev["warning"], p.bySev["info"]), width), bold, color))
		recent := p.ledger
		if len(recent) > 4 {
			recent = recent[len(recent)-4:]
		}
		for _, f := range recent {
			line := fmt.Sprintf("  %-7s  %s:%d  %s", strings.ToUpper(f.Severity), f.File, f.Line, f.Message)
			lines = append(lines, paint(fitText(line, width), severityColor(f.Severity), color))
		}
	}
	return lines
}

func (p *progress) clearLocked() {
	if p.tty && !p.quiet && p.panelH > 0 {
		fmt.Fprintf(os.Stderr, "\033[%dA\r\033[J", p.panelH)
		p.panelH = 0
	}
}

func (p *progress) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
}

func (p *progress) drawBar() {
	p.clearLocked()
	width, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || width < 1 {
		width = 80
	}
	lines := p.lines(max(width-1, 1), colorEnabled(p.tty))
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
	p.panelH = len(lines)
}

func (p *progress) warn(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	if p.tty {
		message = cleanText(message)
	}
	fmt.Fprintln(os.Stderr, message)
	if p.tty && !p.quiet {
		p.drawBar()
	}
}

func printResultFields(w io.Writer, width int, fields []string) {
	line := "  "
	for _, field := range fields {
		separator := ""
		if line != "  " {
			separator = " · "
		}
		if line != "  " && runewidth.StringWidth(line+separator+field) > width {
			fmt.Fprintln(w, line)
			line, separator = "  ", ""
		}
		line += separator + field
	}
	fmt.Fprintln(w, line)
}

func (p *progress) resultsHeader(w io.Writer, findings []Finding, nErr, nIgnored, nSkipped, nSev, exit int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	width := terminalWidth(os.Stdout)
	fmt.Fprintf(w, "  %s\n", paint("RESULTS", bold, colorEnabled(true)))
	printResultFields(w, width, []string{fmt.Sprintf("%d/%d checks", p.done, p.total), fmt.Sprintf("%d findings", len(findings)), formatDuration(time.Since(p.start).Seconds())})
	fields := []string{fmt.Sprintf("%d cached", p.cached), fmt.Sprintf("%d errors", nErr), fmt.Sprintf("%d ignored", nIgnored), fmt.Sprintf("%d skipped", nSkipped)}
	if nSev > 0 {
		fields = append(fields, fmt.Sprintf("%d below severity", nSev))
	}
	fields = append(fields, fmt.Sprintf("exit %d", exit))
	printResultFields(w, width, fields)
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.Severity]++
	}
	printResultFields(w, width, []string{fmt.Sprintf("%d error", counts["error"]), fmt.Sprintf("%d warning", counts["warning"]), fmt.Sprintf("%d info", counts["info"])})
}

func (p *progress) resultsCost() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.costs != nil {
		width := terminalWidth(os.Stderr)
		cost := ansi.Hardwrap(formatCost(p.costs.snapshot()), max(width-2, 1), true)
		fmt.Fprintf(os.Stderr, "  %s\n", paint(strings.ReplaceAll(cost, "\n", "\n  "), dim, colorEnabled(p.tty)))
	}
}

func (p *progress) finish(nFindings, nErr, nIgnored, nSkipped, nSev, exit int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	elapsed := formatDuration(time.Since(p.start).Seconds())
	if p.tty && !p.quiet {
		label, code := "Finished", green
		switch exit {
		case 130:
			label, code = "Interrupted", yellow
		case 2:
			label, code = "Failed", red
		case 1:
			label, code = "Findings", yellow
		}
		color := colorEnabled(p.tty)
		fmt.Fprintf(os.Stderr, "  %s  %d/%d checks  ·  %d findings  ·  %s\n", paint(label, code, color), p.done, p.total, nFindings, elapsed)
		fmt.Fprintf(os.Stderr, "  %s\n", paint(fmt.Sprintf("%d cached  ·  %d errors  ·  %d ignored  ·  %d skipped  ·  %d below severity  ·  exit %d", p.cached, nErr, nIgnored, nSkipped, nSev, exit), dim, color))
		if p.costs != nil {
			fmt.Fprintf(os.Stderr, "  %s\n", paint(formatCost(p.costs.snapshot()), dim, color))
		}
		return
	}
	fmt.Fprintf(os.Stderr, "done: %d/%d checks · %d findings · %d cached · %d errors · %d ignored-inline · %d skipped · %d below-severity · %s · exit %d\n",
		p.done, p.total, nFindings, p.cached, nErr, nIgnored, nSkipped, nSev, elapsed, exit)
	if p.costs != nil {
		fmt.Fprintln(os.Stderr, formatCost(p.costs.snapshot()))
	}
}
