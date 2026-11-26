package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// progress renders the live bar (one redrawn line) and the findings panel
// (most recent 4) on a TTY; on non-TTY it prints a plain line at 25/50/75/100%.
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
	emaSec    float64
	calls     int // non-cached completed calls, for the rate
	last      string
	ledger    []Finding
	ledgerN   int // total findings ever (panel tail line)
	bySev     map[string]int
	milestone int
	panelH    int
}

func newProgress(total int, quiet bool) *progress {
	return &progress{
		tty:   isTerminal(),
		quiet: quiet,
		total: total,
		start: time.Now(),
		bySev: map[string]int{},
	}
}

func isTerminal() bool {
	st, err := os.Stderr.Stat()
	return err == nil && (st.Mode()&os.ModeCharDevice) != 0
}

// tick is called once per finished check. d is the call duration for
// non-cached checks (0 for cached). last describes the check that finished.
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
	if !cached && d > 0 {
		p.calls++
		if p.emaSec == 0 {
			p.emaSec = d.Seconds()
		} else {
			p.emaSec = 0.7*p.emaSec + 0.3*d.Seconds()
		}
	}
	p.last = last
	if p.quiet {
		return
	}
	if !p.tty {
		thresholds := [4]int{25, 50, 75, 100}
		if p.total > 0 && p.milestone < 4 && p.done*100/p.total >= thresholds[p.milestone] {
			p.milestone++
			fmt.Fprintf(os.Stderr, "[%3d%%] %d/%d · %s · eta %s · %d cached\n",
				thresholds[p.milestone-1], p.done, p.total, p.rate(), p.eta(), p.cached)
		}
		return
	}
	p.drawBar()
}

// addFinding shows a new finding in the panel (TTY only) and keeps the tail
// for the panel; the full report still comes at the end.
func (p *progress) addFinding(f Finding) {
	p.mu.Lock()
	p.ledger = append(p.ledger, f)
	p.ledgerN++
	p.bySev[f.Severity]++
	if len(p.ledger) > 8 {
		p.ledger = p.ledger[len(p.ledger)-8:]
	}
	if p.tty && !p.quiet {
		p.drawBar()
	}
	p.mu.Unlock()
}

// noteSubmit marks a check as in flight (queued at the server or being
// decided); the bar shows the queue depth while the pool runs ahead.
func (p *progress) noteSubmit() {
	p.mu.Lock()
	p.inflight++
	p.mu.Unlock()
}

func (p *progress) callsPerSec() float64 {
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
	r := p.callsPerSec()
	if r <= 0 {
		return "—"
	}
	return formatRate(r)
}

func (p *progress) eta() string {
	r := p.callsPerSec()
	if r <= 0 {
		return "—"
	}
	return formatDuration(float64(p.total-p.done) / r)
}

func (p *progress) barLine() string {
	const width = 22
	if p.total == 0 {
		return "decide [no checks]"
	}
	frac := float64(p.done) / float64(p.total)
	filled := int(frac*width + 0.5)
	rest := max(width-filled, 0)
	bar := fmt.Sprintf("decide [%s%s] %3d%%  %d/%d  %s  eta %s  %d cached  %d inflight",
		strings.Repeat("█", filled), strings.Repeat("░", rest),
		int(frac*1000)/10, p.done, p.total, p.rate(), p.eta(), p.cached, p.inflight)
	if p.last != "" {
		if len(bar)+len(p.last)+3 > 120 {
			p.last = ""
		} else {
			bar += "  " + p.last
		}
	}
	return bar
}

func (p *progress) drawBar() {
	if p.panelH > 0 {
		fmt.Fprintf(os.Stderr, "\033[%dA", p.panelH)
	}
	fmt.Fprintf(os.Stderr, "\r%s\033[K\n", p.barLine())
	// findings panel
	var panel []string
	if p.ledgerN > 0 {
		panel = append(panel, fmt.Sprintf(" found %d · %d high %d med %d low",
			p.ledgerN, p.bySev["high"], p.bySev["med"], p.bySev["low"]))
		recent := p.ledger
		if len(recent) > 4 {
			pending := p.ledgerN - 4
			recent = recent[len(recent)-4:]
			panel = append(panel, fmt.Sprintf(" (%d older — full report at the end)", pending))
		}
		for _, f := range recent {
			loc := fmt.Sprintf("%s:%d", f.File, f.Line)
			if len(loc) > 30 {
				loc = "…" + loc[len(loc)-29:]
			}
			reason := f.Message
			if len(reason) > 50 {
				reason = reason[:50] + "…"
			}
			panel = append(panel, fmt.Sprintf("   %-30s %-20s [%s] %s", loc, f.Rule, f.Severity, reason))
		}
	}
	for _, l := range panel {
		fmt.Fprintf(os.Stderr, "%s\033[K\n", l)
	}
	p.panelH = 1 + len(panel)
}

// finish erases the panel and prints the closing summary to stderr.
func (p *progress) finish(nFindings, nErr, nIgnored, nSkipped, nSev, exit int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty && !p.quiet {
		if p.panelH > 0 {
			fmt.Fprintf(os.Stderr, "\033[%dA", p.panelH)
		}
		fmt.Fprintf(os.Stderr, "\r\033[J")
	}
	elapsed := time.Since(p.start).Seconds()
	fmt.Fprintf(os.Stderr, "done: %d/%d checks · %d findings · %d cached · %d errors · %d ignored-inline · %d skipped · %d below-severity · %s · exit %d\n",
		p.done, p.total, nFindings, p.cached, nErr, nIgnored, nSkipped, nSev, formatDuration(elapsed), exit)
}
