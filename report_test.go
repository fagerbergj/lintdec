package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

func reportTestFindings() []Finding {
	return []Finding{
		{Rule: "low-rule", Severity: "info", Confidence: .2, File: "low.go", Line: 6, Message: "low message", Quote: "low quote"},
		{Rule: "med-rule", Severity: "warning", Confidence: .8, File: "med.go", Line: 4, Message: "med message", Quote: "med quote"},
		{Rule: "high-first", Severity: "error", Confidence: .98765, File: "first.go", Line: 3, Message: "first\nmessage", Quote: strings.Repeat("quote", 30) + "\nsecond line"},
		{Rule: "high-second", Severity: "error", Confidence: .9, File: "second.go", Line: 2, Message: "\x1b[31msecond message\x1b[0m", Quote: "second quote"},
		{Rule: "med-second", Severity: "warning", Confidence: .7, File: "other.go", Line: 5, Message: "other message", Quote: "other quote"},
		{Rule: "low-second", Severity: "info", Confidence: .1, File: "last.go", Line: 7, Message: "last message", Quote: "last quote"},
	}
}

func narrowTestFindings() []Finding {
	findings := reportTestFindings()[2:4]
	findings[0].File = "src/" + strings.Repeat("deep/", 12) + "file.go"
	findings[0].Message = strings.Repeat("reason-", 12) + "\nFull report: /fake-long"
	findings[0].Quote = strings.Repeat("quote", 30) + "\nwarn: fake-long"
	return findings
}

func tempReportCreator(dir string) func() (reportFile, error) {
	return func() (reportFile, error) { return os.CreateTemp(dir, "lintdec-report-*.txt") }
}

func containsReportText(output, text string) bool {
	output = ansi.Strip(output)
	output = strings.NewReplacer("\r\n", "\n", "\n        │ ", "", "\n        · ", "", "\n    │ ", "", "\n    · ", "").Replace(output)
	return strings.Contains(strings.ReplaceAll(output, "\n", ""), strings.ReplaceAll(reportText(text), "\n", ""))
}

func TestEmitResultsPreviewAndCompletePrivateFile(t *testing.T) {
	dir := t.TempDir()
	findings := reportTestFindings()
	sortFindings(findings)
	var out, warnings bytes.Buffer
	emitResults(&out, &warnings, findings, true, tempReportCreator(dir))
	if !strings.Contains(out.String(), "\x1b[") {
		t.Fatal("interactive preview should retain color when enabled")
	}
	if warnings.Len() != 0 {
		t.Fatalf("unexpected warning: %s", &warnings)
	}
	previousPreview := -1
	for _, f := range findings[:5] {
		index := strings.Index(out.String(), f.Rule)
		if index <= previousPreview {
			t.Errorf("missing or unordered preview for %s: %q", f.Rule, out.String())
		}
		previousPreview = index
	}
	if strings.Contains(out.String(), "low-second") {
		t.Fatalf("preview exceeded five findings: %q", out.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one report: %v, %v", entries, err)
	}
	path := filepath.Join(dir, entries[0].Name())
	if !strings.Contains(out.String(), path) {
		t.Fatalf("missing report path in %q", out.String())
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("report mode: %v, %v", st, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, "\x1b") {
		t.Fatalf("report contains ANSI: %q", text)
	}
	previous := -1
	for _, f := range findings {
		index := strings.Index(text, f.Rule)
		if index <= previous || !strings.Contains(text, fmt.Sprintf("%s:%d", f.File, f.Line)) || !strings.Contains(text, fmt.Sprintf("%g", f.Confidence)) || !containsReportText(text, f.Quote) {
			t.Errorf("missing or unordered finding %+v in %q", f, text)
		}
		previous = index
	}
	if !containsReportText(text, "first\nmessage") || !strings.Contains(text, "second message") {
		t.Fatalf("messages not preserved: %q", text)
	}
	var second, ignored bytes.Buffer
	emitResults(&second, &ignored, findings, false, tempReportCreator(dir))
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("reports must have unique names: %v, %v", entries, err)
	}
}

func TestEmitResultsRestoresOwnerReadPermission(t *testing.T) {
	dir := t.TempDir()
	var out, warnings bytes.Buffer
	emitResults(&out, &warnings, reportTestFindings(), false, func() (reportFile, error) {
		f, err := os.CreateTemp(dir, "lintdec-report-*.txt")
		if err != nil {
			return nil, err
		}
		if err := f.Chmod(0); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, err
		}
		return f, nil
	})
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || warnings.Len() != 0 {
		t.Fatalf("report creation failed: %v, %v, %q", entries, err, warnings.String())
	}
	st, err := entries[0].Info()
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("report mode: %v, %v", st, err)
	}
}

func TestEmitResultsFiveOrFewerNoFile(t *testing.T) {
	for _, n := range []int{0, 1, 5} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			findings := reportTestFindings()[:n]
			var out, warnings bytes.Buffer
			emitResults(&out, &warnings, findings, false, func() (reportFile, error) {
				t.Fatal("must not create a report")
				return nil, nil
			})
			for _, f := range findings {
				if !strings.Contains(out.String(), f.Rule) {
					t.Errorf("missing %s in %q", f.Rule, out.String())
				}
			}
			if warnings.Len() != 0 || (n == 0 && out.Len() != 0) {
				t.Fatalf("unexpected output: %q, warning: %q", out.String(), warnings.String())
			}
		})
	}
}

type brokenReportFile struct {
	*os.File
	writes, failAfter  int
	chmodErr, closeErr bool
}

func (f *brokenReportFile) Chmod(mode os.FileMode) error {
	if f.chmodErr {
		return errors.New("chmod failed")
	}
	return f.File.Chmod(mode)
}

func (f *brokenReportFile) Write(p []byte) (int, error) {
	if f.failAfter >= 0 && f.writes >= f.failAfter {
		return 0, errors.New("write failed")
	}
	f.writes++
	return f.File.Write(p)
}

func (f *brokenReportFile) Close() error {
	err := f.File.Close()
	if f.closeErr {
		return errors.New("close failed")
	}
	return err
}

func TestEmitResultsFailureFallsBackWithoutDuplicates(t *testing.T) {
	for _, failure := range []string{"create", "chmod", "write", "partial-write", "close"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			findings := reportTestFindings()
			sortFindings(findings)
			var out, warnings bytes.Buffer
			emitResults(&out, &warnings, findings, false, func() (reportFile, error) {
				if failure == "create" {
					return nil, errors.New("create failed")
				}
				f, err := os.CreateTemp(dir, "lintdec-report-*.txt")
				if err != nil {
					return nil, err
				}
				failAfter := -1
				if failure == "write" {
					failAfter = 0
				} else if failure == "partial-write" {
					failAfter = 3
				}
				return &brokenReportFile{File: f, failAfter: failAfter, chmodErr: failure == "chmod", closeErr: failure == "close"}, nil
			})
			if !strings.Contains(warnings.String(), "warn") {
				t.Fatalf("missing warning: %q", warnings.String())
			}
			previous := -1
			for _, f := range findings {
				index := strings.Index(out.String(), f.Rule)
				if index <= previous || strings.Count(out.String(), f.Rule) != 1 || !containsReportText(out.String(), f.Quote) || !containsReportText(out.String(), f.Message) || !strings.Contains(out.String(), fmt.Sprintf("%s:%d", f.File, f.Line)) || !strings.Contains(out.String(), fmt.Sprintf("%g", f.Confidence)) {
					t.Errorf("finding lost, duplicated or unordered: %+v, output %q", f, out.String())
				}
				previous = index
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("partial report not removed: %v, %v", entries, err)
			}
		})
	}
}

func TestReportRedirectedAndJSONRemainUnchanged(t *testing.T) {
	findings := reportTestFindings()
	sortFindings(findings)
	for _, format := range []string{"text", "json", "jsonl"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			out := captureStdout(t, func() { emit(findings, format) })
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("unexpected report file: %v, %v", entries, err)
			}
			if format == "text" {
				var want strings.Builder
				for _, f := range findings {
					fmt.Fprintf(&want, "%s:%d  %-22s [%-7s %.2f] %s\n", f.File, f.Line, f.Rule, f.Severity, f.Confidence, f.Message)
					if f.Quote != "" {
						q := strings.ReplaceAll(f.Quote, "\n", " ")
						if len(q) > 100 {
							q = q[:100] + "…"
						}
						fmt.Fprintf(&want, "        | %s\n", q)
					}
				}
				if out != want.String() {
					t.Fatalf("plain output changed: %q", out)
				}
				return
			}
			var got []Finding
			if format == "json" {
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err)
				}
			} else {
				dec := json.NewDecoder(strings.NewReader(out))
				for {
					var f Finding
					err := dec.Decode(&f)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, f)
				}
			}
			if !slices.Equal(got, findings) {
				t.Fatalf("output changed: %q", out)
			}
		})
	}
}

func TestEmitReportKeepsExitCodesAndProgressCostSummary(t *testing.T) {
	t.Chdir(t.TempDir())
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	original := os.Stderr
	os.Stderr = stderr
	defer func() { os.Stderr = original }()

	for _, tc := range []struct {
		name   string
		result poolResult
		want   int
	}{
		{"clean", poolResult{}, 0},
		{"findings", poolResult{findings: reportTestFindings()}, 1},
		{"errors", poolResult{errs: 2}, 2},
		{"budget", poolResult{findings: reportTestFindings(), budgetSkipped: 1}, 2},
		{"interrupted", poolResult{findings: reportTestFindings(), interrupted: true}, 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prog := newProgress(2, true)
			prog.done = 2
			prog.costs = &costAccumulator{}
			var code int
			out := captureStdout(t, func() { code = emitReport(tc.result, "error", "text", 2, prog, false) })
			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
			if len(tc.result.findings) != 0 {
				if !strings.Contains(out, "high-first") || !strings.Contains(out, "high-second") || strings.Index(out, "high-first") > strings.Index(out, "high-second") || strings.Contains(out, "med-rule") {
					t.Fatalf("filter/sort changed: %q", out)
				}
			}
		})
	}
	content, err := os.ReadFile(stderr.Name())
	if err != nil || !strings.Contains(string(content), "done: 2/2 checks") || !strings.Contains(string(content), "Cost: no model calls") {
		t.Fatalf("missing progress/cost summary: %q, %v", content, err)
	}
}

func TestReportTerminalDispatch(t *testing.T) {
	if os.Getenv("LINTDEC_TEST_REPORT_CHILD") == "1" {
		findings := reportTestFindings()
		sortFindings(findings)
		emit(findings, os.Getenv("LINTDEC_TEST_REPORT_FORMAT"))
		return
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("script PTY invocation is platform-specific")
	}
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script is not installed")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, ci, format string
	}{
		{"interactive", "", "text"},
		{"ci", "true", "text"},
		{"json", "", "json"},
		{"jsonl", "", "jsonl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("LINTDEC_TEST_REPORT_CHILD", "1")
			t.Setenv("LINTDEC_TEST_REPORT_FORMAT", tc.format)
			t.Setenv("CI", tc.ci)
			t.Setenv("TERM", "xterm")
			t.Setenv("NO_COLOR", "1")
			t.Setenv("TMPDIR", dir)
			var cmd *exec.Cmd
			if runtime.GOOS == "darwin" {
				cmd = exec.Command(script, append([]string{"-q", "/dev/null", executable}, childTestArgs(t, "TestReportTerminalDispatch")...)...)
			} else {
				quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
				cmd = exec.Command(script, "-q", "-e", "-c", quoted+" "+strings.Join(childTestArgs(t, "TestReportTerminalDispatch"), " "), "/dev/null")
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("PTY child failed: %v: %q", err, output)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("unexpected files: %v, %v; output %q", entries, err, output)
			}
			for _, f := range reportTestFindings() {
				if strings.Count(string(output), f.Rule) != 1 {
					t.Fatalf("missing or duplicated %s: %q", f.Rule, output)
				}
			}
		})
	}
}

func TestResultsReportPathSanitizesControls(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unsafe\x1b[31m\nname")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	findings := reportTestFindings()
	sortFindings(findings)
	var out, warnings bytes.Buffer
	emitResults(&out, &warnings, findings, false, tempReportCreator(dir))
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || warnings.Len() != 0 {
		t.Fatalf("report creation failed: %v, %v, %q", entries, err, warnings.String())
	}
	path := filepath.Join(dir, entries[0].Name())
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "Full report: "+strconv.Quote(path)) {
		t.Fatalf("unsafe or inaccessible path: %q", out.String())
	}
}

func TestFindingTableHeadingScoresAndIndentation(t *testing.T) {
	findings := reportTestFindings()
	sortFindings(findings)
	var out bytes.Buffer
	renderFindingTable(&out, findings, 80, false)
	text := out.String()
	for _, want := range []string{"  FINDINGS · 5 of 6", "Level", "Confidence", "Location", "Rule", "0.99", "0.90", "0.80", "0.70", "0.20"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "low-second") || strings.Contains(text, "0.98765") {
		t.Fatalf("preview includes overflow or unrounded scores: %q", text)
	}
	if !strings.Contains(text, "┌") || !strings.Contains(text, "┘") {
		t.Fatalf("normal-width preview must be bordered: %q", text)
	}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if !strings.HasPrefix(line, "  ") || runewidth.StringWidth(line) > 80 {
			t.Errorf("table line is not indented or exceeds width: %q", line)
		}
	}
}

func TestFindingTableColorPreservesLayout(t *testing.T) {
	findings := reportTestFindings()
	sortFindings(findings)
	for _, width := range []int{30, 80} {
		var colored, plain bytes.Buffer
		renderFindingTable(&colored, findings, width, true)
		renderFindingTable(&plain, findings, width, false)
		if !strings.Contains(colored.String(), "\x1b[") || ansi.Strip(colored.String()) != plain.String() {
			t.Fatalf("color changed width-%d layout: colored=%q, plain=%q", width, colored.String(), plain.String())
		}
	}
}

func TestFindingTableFitsNarrowTerminals(t *testing.T) {
	findings := reportTestFindings()
	sortFindings(findings)
	findings[0].File = "目录/" + strings.Repeat("long", 10) + "\x1b[2J.go"
	location := strconv.Quote(findings[0].File) + ":3"
	for _, width := range []int{5, 9, 11, 15, 20, 26, 30, 40, 60, 100} {
		var out bytes.Buffer
		renderFindingTable(&out, findings[:5], width, false)
		for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			isLocation := false
			for _, f := range findings[:5] {
				isLocation = isLocation || strings.Contains(line, reportLocation(f.File, f.Line))
			}
			if got := runewidth.StringWidth(line); got > width && !isLocation {
				t.Errorf("width %d: line has width %d: %q", width, got, line)
			}
		}
		if !strings.Contains(out.String(), location) {
			t.Fatalf("width %d: location is not copyable: %q", width, out.String())
		}
		if strings.Contains(out.String(), "\x1b") {
			t.Fatalf("untrusted control in table: %q", out.String())
		}
		if width >= 30 {
			for _, label := range []string{"FINDINGS · 5 of 5", "Level", "Confidence", "Location", "Rule"} {
				if !strings.Contains(out.String(), label) {
					t.Errorf("width %d missing %s: %q", width, label, out.String())
				}
			}
		}
	}
}

func TestFindingTableKeepsLongUnicodeAndSanitizesControls(t *testing.T) {
	f := Finding{Severity: "error", Confidence: .98765, File: "目录/" + strings.Repeat("汉", 45) + "/終.go\x1b[2J", Line: 73, Rule: "R"}
	for _, width := range []int{30, 40, 60, 80, 100} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			var out bytes.Buffer
			renderFindingTable(&out, []Finding{f}, width, false)
			text := out.String()
			if strings.Contains(text, "\x1b") || strings.Count(text, "汉") != 45 || !strings.Contains(text, "目录") || !strings.Contains(text, "終") || !strings.Contains(text, "0.99") {
				t.Fatalf("unicode, score or control safety lost: %q", text)
			}
			location := strconv.Quote(f.File) + ":73"
			if !strings.Contains(text, location) {
				t.Fatalf("location is not copyable: %q", text)
			}
			for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
				if runewidth.StringWidth(line) > width && !strings.Contains(line, location) {
					t.Fatalf("line exceeds width %d: %q", width, line)
				}
			}
		})
	}
}

func TestFindingTableLongPathIsCopyable(t *testing.T) {
	finding := Finding{Severity: "error", Confidence: .99, File: "目录/" + strings.Repeat("long", 10) + ".go", Line: 3, Rule: "high-first"}
	var out bytes.Buffer
	renderFindingTable(&out, []Finding{finding, {Severity: "info", Confidence: .2, File: "low.go", Line: 6, Rule: "low-rule"}}, 80, false)
	text := out.String()
	if !strings.Contains(text, "high-first") || !strings.Contains(text, finding.File+":3") {
		t.Fatalf("table split or clipped path: %q", text)
	}
}

func TestResultsLayoutPTY(t *testing.T) {
	if os.Getenv("LINTDEC_RESULTS_CHILD") == "1" {
		if err := os.Chdir(os.Getenv("LINTDEC_RESULTS_WORKDIR")); err != nil {
			t.Fatal(err)
		}
		if redirected := os.Getenv("LINTDEC_RESULTS_REDIRECT"); redirected != "" {
			f, err := os.Create("redirect-output")
			if err != nil {
				t.Fatal(err)
			}
			// Restore the real stream before closing: a coverage build writes its summary to stdout at exit.
			orig := &os.Stdout
			if redirected != "stdout" {
				orig = &os.Stderr
			}
			saved := *orig
			*orig = f
			defer func() { *orig = saved; f.Close() }()
		}
		findings := reportTestFindings()
		mode := os.Getenv("LINTDEC_RESULTS_MODE")
		if mode == "narrow" || mode == "narrow-overflow" || mode == "narrow-cost" || mode == "narrow-fallback" {
			cmd := exec.Command("stty", "cols", "30")
			cmd.Stdin = os.Stdout
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("set PTY width: %v: %s", err, output)
			}
		}
		if mode == "narrow" {
			findings = narrowTestFindings()
		} else if mode == "narrow-fallback" {
			findings[2] = narrowTestFindings()[0]
		}
		if mode == "relative" {
			if err := os.Mkdir("relative", 0o700); err != nil {
				t.Fatal(err)
			}
		}
		result := poolResult{findings: findings, errs: 1, ignoredCount: 2}
		sev := "info"
		switch mode {
		case "small", "narrow", "narrow-cost":
			result.findings = findings[:2]
		case "five":
			result.findings = findings[:5]
		case "empty":
			result.findings = nil
		case "budget":
			result.budgetSkipped = 2
		case "interrupted":
			result.interrupted = true
		case "filtered":
			sev = "error"
		}
		prog := newProgress(6, mode == "quiet")
		prog.done, prog.cached, prog.start = 6, 1, time.Now().Add(-3*time.Second)
		prog.costs = &costAccumulator{}
		if mode == "narrow-cost" {
			usd := 0.02
			prog.costs.record(requestAccounting{costUSD: &usd, costSource: "provider"})
		}
		if prog.tty && !prog.quiet {
			fmt.Fprintln(os.Stderr, "old-progress")
			prog.panelH = 1
		}
		create := func() (reportFile, error) {
			if mode == "create" || mode == "narrow-fallback" {
				return nil, errors.New("create failure")
			}
			f, err := os.CreateTemp("", "lintdec-report-*.txt")
			if err != nil {
				return nil, err
			}
			failAfter := -1
			if mode == "write" {
				failAfter = 0
			}
			if mode == "partial-write" {
				failAfter = 3
			}
			return &brokenReportFile{File: f, failAfter: failAfter, chmodErr: mode == "chmod", closeErr: mode == "close"}, nil
		}
		wantExit := 1
		if mode == "empty" {
			wantExit = 0
		}
		if mode == "budget" {
			wantExit = 2
		}
		if mode == "interrupted" {
			wantExit = 130
		}
		format := os.Getenv("LINTDEC_RESULTS_FORMAT")
		if format == "" {
			format = "text"
		}
		if code := emitReportWithTemp(result, sev, format, 6, prog, false, create); code != wantExit {
			t.Fatalf("exit=%d, want %d", code, wantExit)
		}
		return
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("script PTY invocation is platform-specific")
	}
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script is not installed")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"overflow", "relative", "small", "narrow", "narrow-overflow", "narrow-cost", "narrow-fallback", "five", "empty", "filtered", "budget", "interrupted", "create", "chmod", "write", "partial-write", "close"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			work := t.TempDir()
			t.Setenv("LINTDEC_RESULTS_CHILD", "1")
			t.Setenv("LINTDEC_RESULTS_MODE", mode)
			t.Setenv("LINTDEC_RESULTS_WORKDIR", work)
			t.Setenv("LINTDEC_RESULTS_REDIRECT", "")
			t.Setenv("LINTDEC_RESULTS_FORMAT", "")
			t.Setenv("CI", "")
			t.Setenv("TERM", "xterm")
			t.Setenv("NO_COLOR", "1")
			tmp := dir
			if mode == "relative" {
				tmp = "relative"
			}
			t.Setenv("TMPDIR", tmp)
			var cmd *exec.Cmd
			if runtime.GOOS == "darwin" {
				cmd = exec.Command(script, append([]string{"-q", "/dev/null", executable}, childTestArgs(t, "TestResultsLayoutPTY")...)...)
			} else {
				quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
				cmd = exec.Command(script, "-q", "-e", "-c", quoted+" "+strings.Join(childTestArgs(t, "TestResultsLayoutPTY"), " "), "/dev/null")
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("PTY child failed: %v: %q", err, output)
			}
			text := strings.ReplaceAll(string(output), "\r\n", "\n")
			results := strings.Index(text, "RESULTS")
			checks := strings.Index(text, "6/6 checks")
			counts := strings.Index(text, " error · ")
			cost := strings.Index(text, "Cost: ")
			if results < 0 || checks <= results || counts <= checks || cost <= counts || strings.Count(text, "RESULTS") != 1 {
				t.Fatalf("incorrect results ordering: %q", text)
			}
			if mode == "narrow" || mode == "narrow-overflow" || mode == "narrow-cost" || mode == "narrow-fallback" {
				locations := reportTestFindings()
				if mode == "narrow" {
					locations = narrowTestFindings()
				} else if mode == "narrow-fallback" {
					locations[2] = narrowTestFindings()[0]
				}
				for _, line := range strings.Split(text[results:], "\n") {
					isPath := strings.Contains(line, "Full report: ")
					for _, f := range locations {
						location := reportLocation(f.File, f.Line)
						isPath = isPath || (runewidth.StringWidth(location)+2 > 30 && strings.Contains(line, location))
					}
					if runewidth.StringWidth(line) > 30 && !isPath {
						t.Errorf("narrow output exceeded 30 columns: %q", line)
					}
				}
				if mode == "narrow" && !strings.Contains(text, reportLocation(locations[0].File, locations[0].Line)) {
					t.Fatalf("narrow file path is not copyable: %q", text)
				}
				if mode == "narrow-fallback" && !strings.Contains(text, reportLocation(locations[2].File, locations[2].Line)) {
					t.Fatalf("fallback file path is not copyable: %q", text)
				}
				if mode == "narrow-cost" && !strings.Contains(strings.ReplaceAll(text[cost:], "\n  ", ""), "Cost: $0.020000 provider-reported (USD; other charges may apply)") {
					t.Fatalf("narrow cost lost content: %q", text[cost:])
				}
			}
			if !strings.Contains(text, "1 cached") || !strings.Contains(text, "1 errors") || !strings.Contains(text, "2 ignored") || !strings.Contains(text, " skipped") {
				t.Fatalf("missing counters: %q", text)
			}
			if mode == "filtered" && !strings.Contains(text, "4 below severity") {
				t.Fatalf("missing severity suppression: %q", text)
			}
			if mode != "filtered" && strings.Contains(text, "below severity") {
				t.Fatalf("zero suppressed count should be omitted: %q", text)
			}
			if mode == "budget" && (!strings.Contains(text, "2 skipped") || !strings.Contains(text, "exit 2")) {
				t.Fatalf("budget state missing: %q", text)
			}
			if mode == "interrupted" && !strings.Contains(text, "exit 130") {
				t.Fatalf("interruption missing: %q", text)
			}
			failure := mode == "create" || mode == "chmod" || mode == "write" || mode == "partial-write" || mode == "close" || mode == "narrow-fallback"
			reportDir := dir
			if mode == "relative" {
				reportDir, err = filepath.EvalSymlinks(filepath.Join(work, "relative"))
				if err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(reportDir)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "overflow" || mode == "relative" || mode == "budget" || mode == "interrupted" || mode == "narrow-overflow" {
				if len(entries) != 1 {
					t.Fatalf("missing full report: %v", entries)
				}
				path := filepath.Join(reportDir, entries[0].Name())
				pathIndex := strings.Index(text, "Full report: "+path)
				table := strings.Index(text, "Level")
				if table <= counts || pathIndex <= table || cost <= pathIndex || strings.Contains(text, "low-second") {
					t.Fatalf("overflow layout: %q", text)
				}
				previous := table
				findings := reportTestFindings()
				sortFindings(findings)
				if mode != "narrow-overflow" {
					for _, f := range findings[:5] {
						index := strings.Index(text, f.Rule)
						if index <= previous {
							t.Errorf("preview is not sorted: %q", text)
						}
						previous = index
					}
				}
				file, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o600 || strings.Contains(string(file), "\x1b") {
					t.Fatalf("report not private or contains ANSI: %v, %v", info, err)
				}
				for _, f := range findings {
					if !strings.Contains(string(file), f.Rule) || !containsReportText(string(file), f.Quote) || !containsReportText(string(file), f.Message) {
						t.Errorf("complete report lost %s", f.Rule)
					}
				}
			} else if len(entries) != 0 {
				t.Fatalf("unexpected report: %v", entries)
			}
			if failure {
				if !strings.Contains(text, "warn:") || strings.Contains(text, "\n  Full report: ") {
					t.Fatalf("fallback missing warning: %q", text)
				}
				findings := reportTestFindings()
				if mode == "narrow-fallback" {
					findings[2] = narrowTestFindings()[0]
				}
				for _, f := range findings {
					if strings.Count(text, f.Rule) != 1 || !containsReportText(text, f.Quote) || !containsReportText(text, f.Message) {
						t.Errorf("fallback lost/duplicated %s: %q", f.Rule, text)
					}
				}
				if mode == "narrow-fallback" {
					for _, line := range strings.Split(text, "\n") {
						if (strings.Contains(line, "/fake-long") || strings.Contains(line, "warn: fake-long")) && !strings.Contains(line, "│ ") && !strings.Contains(line, "· ") {
							t.Errorf("untrusted fallback line can impersonate status: %q", line)
						}
					}
				}
			} else if mode != "empty" {
				table := strings.Index(text, "Level")
				shown, total := 5, 6
				if mode == "small" || mode == "narrow" || mode == "narrow-cost" || mode == "filtered" {
					shown, total = 2, 2
				} else if mode == "five" {
					shown, total = 5, 5
				}
				heading := strings.Index(text, fmt.Sprintf("FINDINGS · %d of %d", shown, total))
				if heading <= counts || table <= heading || cost <= table {
					t.Fatalf("missing ordered table: %q", text)
				}
				for _, col := range []string{"Confidence", "Location", "Rule"} {
					if !strings.Contains(text, col) {
						t.Errorf("missing %s: %q", col, text)
					}
				}
				if mode == "small" || mode == "narrow" || mode == "narrow-cost" || mode == "five" {
					findings := reportTestFindings()
					if mode == "narrow" {
						findings = narrowTestFindings()
					} else if mode == "small" || mode == "narrow-cost" {
						findings = findings[:2]
					} else {
						findings = findings[:5]
					}
					for _, f := range findings {
						if !containsReportText(text, f.Message) || !containsReportText(text, f.Quote) {
							t.Fatalf("small report lost details for %s: %q", f.Rule, text)
						}
					}
					if mode == "narrow" {
						for _, line := range strings.Split(text, "\n") {
							if (strings.Contains(line, "/fake-long") || strings.Contains(line, "warn: fake-long")) && !strings.Contains(line, "│ ") && !strings.Contains(line, "· ") {
								t.Errorf("untrusted line can impersonate status: %q", line)
							}
						}
					}
				}
			}
		})
	}
}

func TestResultsLayoutPreservesOtherModesPTY(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("script PTY invocation is platform-specific")
	}
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script is not installed")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mode, redirect, format, ci string
		failTemp                         bool
	}{
		{"stdout-redirected", "overflow", "stdout", "text", "", false},
		{"stderr-redirected", "overflow", "stderr", "text", "", false},
		{"stderr-redirected-no-temp", "overflow", "stderr", "text", "", true},
		{"quiet", "quiet", "", "text", "", false},
		{"quiet-no-temp", "quiet", "", "text", "", true},
		{"ci", "overflow", "", "text", "true", false},
		{"json", "overflow", "", "json", "", false},
		{"jsonl", "overflow", "", "jsonl", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, work := t.TempDir(), t.TempDir()
			t.Setenv("LINTDEC_RESULTS_CHILD", "1")
			t.Setenv("LINTDEC_RESULTS_MODE", tc.mode)
			t.Setenv("LINTDEC_RESULTS_REDIRECT", tc.redirect)
			t.Setenv("LINTDEC_RESULTS_FORMAT", tc.format)
			t.Setenv("LINTDEC_RESULTS_WORKDIR", work)
			t.Setenv("CI", tc.ci)
			t.Setenv("TERM", "xterm")
			t.Setenv("NO_COLOR", "1")
			tmp := dir
			if tc.failTemp {
				tmp = filepath.Join(dir, "missing")
			}
			t.Setenv("TMPDIR", tmp)
			var cmd *exec.Cmd
			if runtime.GOOS == "darwin" {
				cmd = exec.Command(script, append([]string{"-q", "/dev/null", executable}, childTestArgs(t, "TestResultsLayoutPTY")...)...)
			} else {
				quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
				cmd = exec.Command(script, "-q", "-e", "-c", quoted+" "+strings.Join(childTestArgs(t, "TestResultsLayoutPTY"), " "), "/dev/null")
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("PTY child failed: %v: %q", err, output)
			}
			terminalOutput := string(output)
			redirectOutput := ""
			if tc.redirect != "" {
				data, err := os.ReadFile(filepath.Join(work, "redirect-output"))
				if err != nil {
					t.Fatal(err)
				}
				redirectOutput = string(data)
			}
			if strings.Contains(terminalOutput, "RESULTS") || strings.Contains(redirectOutput, "RESULTS") {
				t.Fatalf("unified layout unexpectedly used: terminal=%q redirected=%q", terminalOutput, redirectOutput)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("unexpected files: %v, %v", entries, err)
			}
			if tc.failTemp && (strings.Contains(terminalOutput, "warn:") || strings.Contains(redirectOutput, "warn:")) {
				t.Fatalf("plain mode tried to create a report: %q / %q", terminalOutput, redirectOutput)
			}
			if tc.format == "text" && tc.redirect != "stdout" {
				findings := reportTestFindings()
				sortFindings(findings)
				var original bytes.Buffer
				renderFindings(&original, findings, false)
				if !strings.Contains(strings.ReplaceAll(terminalOutput, "\r\n", "\n"), original.String()) {
					t.Fatalf("plain TTY output changed: %q", terminalOutput)
				}
				for _, f := range reportTestFindings() {
					if strings.Count(terminalOutput, f.Rule) != 1 {
						t.Fatalf("plain report lost or duplicated %s: %q", f.Rule, terminalOutput)
					}
				}
				if strings.Contains(terminalOutput, "Full report:") || strings.Contains(terminalOutput, "Complete report") {
					t.Fatalf("plain report unexpectedly created a preview: %q", terminalOutput)
				}
			}
			switch tc.redirect {
			case "stdout":
				if !strings.Contains(redirectOutput, "last.go:7") || !strings.Contains(terminalOutput, "6/6 checks") || !strings.Contains(terminalOutput, "Cost: no model calls") {
					t.Fatalf("redirected stdout behavior changed: %q / %q", terminalOutput, redirectOutput)
				}
			case "stderr":
				if !strings.Contains(redirectOutput, "done: 6/6 checks") || !strings.Contains(redirectOutput, "Cost: no model calls") || !strings.Contains(terminalOutput, "low-second") {
					t.Fatalf("redirected stderr behavior changed: %q / %q", terminalOutput, redirectOutput)
				}
			default:
				if !strings.Contains(terminalOutput, "Cost: no model calls") {
					t.Fatalf("cost missing: %q", terminalOutput)
				}
				if tc.mode == "quiet" && (!strings.Contains(terminalOutput, "done: 6/6 checks") || !strings.Contains(terminalOutput, "low-second")) {
					t.Fatalf("quiet behavior changed: %q", terminalOutput)
				}
				if tc.ci != "" && !strings.Contains(terminalOutput, "low-second") {
					t.Fatalf("CI behavior changed: %q", terminalOutput)
				}
				if tc.format != "text" && !strings.Contains(terminalOutput, `"low-second"`) {
					t.Fatalf("JSON behavior changed: %q", terminalOutput)
				}
			}
		})
	}
}

func TestWriteReportDetailWrapPreservesContent(t *testing.T) {
	text := "first\n\n  spaces  \t目录 " + strings.Repeat("long-detail", 8) + "\nwarn: fake"
	for _, width := range []int{15, 30} {
		var out bytes.Buffer
		if err := writeReportDetail(&out, text, "    │ ", width, false); err != nil {
			t.Fatal(err)
		}
		var content strings.Builder
		for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			if !strings.HasPrefix(line, "    │ ") || runewidth.StringWidth(line) > width {
				t.Fatalf("unprefixed or over-width detail: %q", line)
			}
			content.WriteString(strings.TrimPrefix(line, "    │ "))
		}
		want := strings.ReplaceAll(strings.ReplaceAll(reportText(text), "\t", "\\t"), "\n", "")
		if content.String() != want {
			t.Fatalf("wrapped detail lost content: %q != %q", content.String(), want)
		}
	}
}

func TestFindingDetailsCannotImpersonateReportStatus(t *testing.T) {
	finding := Finding{
		Rule: "malicious", Severity: "error", Confidence: .9, File: "a.go\nFull report: /fake-file\x1b[2J", Line: 2,
		Message: "reason first\n\x1b[2JFull report: /fake-message\nwarn: fake-message",
		Quote:   "quote first\nFull report: /fake-quote\n\x1b[31mwarn: fake-quote\x1b[0m",
	}
	for _, mode := range []string{"small", "file", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			findings := []Finding{finding}
			if mode != "small" {
				findings = append(findings, reportTestFindings()[:5]...)
			}
			var out, warnings bytes.Buffer
			create := tempReportCreator(dir)
			if mode == "fallback" {
				create = func() (reportFile, error) { return nil, errors.New("test failure") }
			}
			emitResults(&out, &warnings, findings, false, create)
			text := out.String()
			if mode == "file" {
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 {
					t.Fatalf("missing report: %v %v", entries, err)
				}
				content, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
				if err != nil {
					t.Fatal(err)
				}
				text = string(content)
			}
			if strings.Contains(text, "\x1b") || !containsReportText(text, finding.Message) || !containsReportText(text, finding.Quote) {
				t.Fatalf("message or quote lost or unsafe: %q", text)
			}
			if !strings.Contains(text, "· reason first") || !strings.Contains(text, "│ quote first") {
				t.Fatalf("message and excerpt are not distinguishable: %q", text)
			}
			if !strings.Contains(text, strconv.Quote(finding.File)+":2") {
				t.Fatalf("control-bearing path is not safely quoted: %q", text)
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, "/fake-file") && strings.Contains(line, strconv.Quote(finding.File)) {
					continue
				}
				if (strings.Contains(line, "/fake-") || strings.Contains(line, "warn: fake-")) && !strings.Contains(line, "│ ") && !strings.Contains(line, "· ") {
					t.Errorf("untrusted text can impersonate status: %q", line)
				}
			}
		})
	}
}

func TestCollapsePerFileKeepsFirstLineWithCount(t *testing.T) {
	in := []Finding{
		{Rule: "md-wordy-phrase", File: "a.md", Line: 9, Message: "wordy"},
		{Rule: "md-wordy-phrase", File: "a.md", Line: 3, Message: "wordy"},
		{Rule: "md-wordy-phrase", File: "b.md", Line: 1, Message: "wordy"},
		{Rule: "md-marketing", File: "a.md", Line: 5, Message: "hype"},
		{Rule: "md-marketing", File: "a.md", Line: 6, Message: "hype"},
	}
	got := collapsePerFile(in)
	if len(got) != 4 || got[0].Line != 3 || got[0].Message != "wordy (2 places in this file)" || got[1].Message != "wordy" {
		t.Fatalf("collapsePerFile = %+v", got)
	}
}

// childTestArgs: flags for a re-executed test child. A coverage build writes its data at exit,
// so it gets a real directory even when the test breaks TMPDIR.
func childTestArgs(t *testing.T, name string) []string {
	args := []string{"-test.run=^" + name + "$"}
	if testing.CoverMode() != "" {
		args = append(args, "-test.gocoverdir="+t.TempDir())
	}
	return args
}
