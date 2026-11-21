package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	iofs "io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

type job struct {
	unit  Unit
	rules []Rule // one model call decides all of these
}

func jobKey(j job) string {
	ids := make([]string, 0, len(j.rules))
	for _, r := range j.rules {
		ids = append(ids, r.ID)
	}
	return fmt.Sprintf("%s|%s|%d|%d", strings.Join(ids, ","), j.unit.File, j.unit.Start, j.unit.End)
}

// reorderArgs moves flags (with their values) ahead of positionals so the
// user can write either `lintdec --out jsonl pkg/` or `lintdec pkg/ --out jsonl`.
func reorderArgs(args []string) []string {
	valueFlags := map[string]bool{
		"rules": true, "severity": true, "out": true, "provider": true, "base-url": true,
		"model": true, "api-key": true, "workers": true, "budget": true, "temperature": true,
		"ledger": true, "include": true, "exclude": true,
	}
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			name := strings.TrimLeft(a, "-")
			if eq := strings.Index(name, "="); eq >= 0 {
				name = name[:eq]
			}
			flags = append(flags, a)
			if !strings.Contains(a, "=") && valueFlags[name] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

var skipDirs = map[string]bool{".git": true, "vendor": true, "node_modules": true, ".lintdec": true}

// isWorktree: a git worktree's .git is a file (a gitdir pointer), a
// checkout's is a directory. Linting a worktree duplicates every finding
// of the primary checkout.
func isWorktree(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && !st.IsDir()
}

// commonAncestor returns the shared directory prefix of two paths.
func commonAncestor(a, b string) (string, bool) {
	aSegs := strings.Split(filepath.ToSlash(a), "/")
	bSegs := strings.Split(filepath.ToSlash(b), "/")
	n := min(len(aSegs), len(bSegs))
	i := 0
	for i < n && aSegs[i] == bSegs[i] {
		i++
	}
	if i == 0 {
		return ".", false
	}
	return strings.Join(aSegs[:i], "/"), true
}

// collectFiles turns positional args (files, dirs, or glob patterns) into a
// file list. Explicitly named files bypass include/exclude.
func collectFiles(paths, include, exclude []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string, explicit bool) {
		p = filepath.ToSlash(p)
		if rel, err := filepath.Rel(".", p); err == nil {
			p = rel
		}
		if strings.HasPrefix(p, "..") {
			return
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			return
		}
		if !explicit {
			if matchAnyGlob(exclude, p) {
				return
			}
			if len(include) > 0 && !matchAnyGlob(include, p) {
				return
			}
		}
		if st.Size() > 1<<20 {
			fmt.Fprintf(os.Stderr, "warn: %s: over 1MB, skipped\n", p)
			return
		}
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	walk := func(root string, fn func(string)) {
		filepath.WalkDir(root, func(p string, d iofs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != root && skipDirs[filepath.Base(p)] {
					return filepath.SkipDir
				}
				if isWorktree(p) {
					fmt.Fprintf(os.Stderr, "skip: %s is a git worktree of another checkout (pass specific files to lint it)\n", p)
					return filepath.SkipDir
				}
				return nil
			}
			fn(p)
			return nil
		})
	}
	for _, raw := range paths {
		st, err := os.Stat(raw)
		if err == nil && !st.IsDir() {
			add(raw, true)
			continue
		}
		if err == nil {
			walk(raw, func(p string) { add(p, false) })
			continue
		}
		matched := 0
		walk(".", func(p string) {
			rel, err := filepath.Rel(".", p)
			if err != nil {
				return
			}
			if matchGlob(raw, filepath.ToSlash(rel)) {
				add(p, false)
				matched++
			}
		})
		if matched == 0 {
			return nil, fmt.Errorf("no files matched: %s", raw)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no lintable files found")
	}
	return out, nil
}

// buildJobs turns one unit into its model batches: all applicable rules in
// one call when the unit fits; otherwise windows for local rules and a
// digest for whole rules, so each batch sees one consistent view.
func buildJobs(u Unit, active []Rule, lines []string) []job {
	var app []Rule
	for _, r := range active {
		if ruleApplies(r, u.Kind) {
			app = append(app, r)
		}
	}
	if len(app) == 0 {
		return nil
	}
	if u.Kind == "File" || u.End-u.Start+1 <= rawBudgetLines {
		return []job{{u, app}}
	}
	var local, whole []Rule
	for _, r := range app {
		if r.Scope == "local" {
			local = append(local, r)
		} else {
			whole = append(whole, r)
		}
	}
	var out []job
	for start := u.Start; start <= u.End; start += rawBudgetLines {
		end := min(start+rawBudgetLines-1, u.End)
		wu := u
		wu.Start, wu.End = start, end
		wu.Text = strings.Join(lines[start-1:end], "\n")
		if len(local) > 0 {
			out = append(out, job{wu, local})
		}
	}
	if len(whole) > 0 {
		du := u
		du.Text = digestFor(u, lines)
		out = append(out, job{du, whole})
	}
	return out
}

func lintCmd(args []string) int {
	args = reorderArgs(args)
	fl := flag.NewFlagSet("lint", flag.ContinueOnError)
	var fRules, fSeverity, fOut, fProvider, fBaseURL, fModel, fAPIKey, fInclude, fExclude, fLedger string
	var fWorkers, fBudget int
	var fTemp float64
	var fNoCache, fNoIgnore, fQuiet, fRetryErrors, fDumpUnits bool
	fl.StringVar(&fRules, "rules", "", "comma-separated rule ids (default all)")
	fl.StringVar(&fSeverity, "severity", "", "min severity to report: low|med|high")
	fl.StringVar(&fOut, "out", "text", "output: text|json|jsonl")
	fl.StringVar(&fProvider, "provider", "", "provider name from the global config")
	fl.StringVar(&fBaseURL, "base-url", "", "OpenAI-compatible base URL")
	fl.StringVar(&fModel, "model", "", "model id")
	fl.StringVar(&fAPIKey, "api-key", "", "API key")
	fl.IntVar(&fWorkers, "workers", 8, "concurrent in-flight batches (server-side gateways cap concurrency; 8 is a safe default, raise it and expect 429 backoff)")
	fl.IntVar(&fBudget, "budget", 0, "max model calls this run, 0 = unlimited")
	fl.Float64Var(&fTemp, "temperature", 0, "sampling temperature")
	fl.BoolVar(&fNoCache, "no-cache", false, "bypass .lintdec/cache")
	fl.BoolVar(&fNoIgnore, "no-ignore", false, "report findings despite inline //lintdec:ignore")
	fl.BoolVar(&fQuiet, "quiet", false, "no progress output")
	fl.StringVar(&fLedger, "ledger", "", "append findings as JSONL to FILE as decided")
	fl.BoolVar(&fRetryErrors, "retry-errors", false, "re-run only checks that errored last run")
	fl.BoolVar(&fDumpUnits, "dump-units", false, "print split units and exit (no model calls)")
	fl.StringVar(&fInclude, "include", "", "comma-separated glob include filters")
	fl.StringVar(&fExclude, "exclude", "", "comma-separated glob exclude filters")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	setFlags := map[string]bool{}
	fl.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	if fSeverity == "" {
		fSeverity = "low"
	}
	if severityRank(fSeverity) < 0 {
		fmt.Fprintf(os.Stderr, "bad --severity: %s (low|med|high)\n", fSeverity)
		return 2
	}
	include := []string{"*.go", "*.md"}
	if fInclude != "" {
		include = strings.Split(fInclude, ",")
	}
	var exclude []string
	if fExclude != "" {
		exclude = strings.Split(fExclude, ",")
	}

	// Project config: flag > env > lintdec.yaml > global config. The yaml is
	// discovered from the linted paths' common ancestor, not the CWD, so
	// `lintdec quack` run from anywhere finds quack's policy.
	paths := fl.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	base := "."
	for i, p := range paths {
		if i == 0 {
			base = p
			continue
		}
		if a, ok := commonAncestor(base, p); ok {
			base = a
		}
	}
	if st, err := os.Stat(base); err == nil && !st.IsDir() {
		base = filepath.Dir(base)
	}
	pc, pcWarn := loadProjectConfig(base)
	for _, w := range pcWarn {
		fmt.Fprintln(os.Stderr, "warn: "+w)
	}
	if !setFlags["severity"] && pc.Severity != "" {
		fSeverity = pc.Severity
	}
	if fInclude == "" && len(pc.Include) > 0 {
		include = pc.Include
	}
	if fExclude == "" && len(pc.Exclude) > 0 {
		exclude = pc.Exclude
	}
	active := make([]Rule, 0, len(rules))
	for _, r := range rules {
		active = append(active, r)
	}
	if fRules != "" {
		var want []Rule
		for id := range strings.SplitSeq(fRules, ",") {
			if r, ok := findRule(strings.TrimSpace(id)); ok {
				want = append(want, r)
			} else {
				fmt.Fprintf(os.Stderr, "unknown rule: %s\n", id)
				return 2
			}
		}
		active = want
	} else {
		if len(pc.RulesOn) > 0 {
			var on []Rule
			for _, r := range active {
				if slices.Contains(pc.RulesOn, r.ID) {
					on = append(on, r)
				}
			}
			active = on
		}
		var off []Rule
		for _, r := range active {
			if !slices.Contains(pc.RulesOff, r.ID) {
				off = append(off, r)
			}
		}
		active = off
	}

	// Provider resolution.
	var prov Provider
	if fBaseURL != "" {
		prov = Provider{BaseURL: strings.TrimRight(fBaseURL, "/"), Model: fModel, APIKey: fAPIKey}
	} else {
		name := fProvider
		if name == "" {
			name = os.Getenv("LINTDEC_PROVIDER")
		}
		if name == "" {
			name = pc.Provider
		}
		cfg, err := loadConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if name == "" {
			name = cfg.Active
		}
		if name == "" {
			fmt.Fprintln(os.Stderr, "no provider configured; run: lintdec provider add")
			return 2
		}
		p, ok := cfg.Providers[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "no such provider: %s\n", name)
			return 2
		}
		prov = p
	}
	if e := os.Getenv("LINTDEC_BASE_URL"); e != "" && fBaseURL == "" {
		prov.BaseURL = strings.TrimRight(e, "/")
	}
	if fModel == "" {
		if e := os.Getenv("LINTDEC_MODEL"); e != "" {
			prov.Model = e
		} else if pc.Model != "" {
			prov.Model = pc.Model
		}
	}
	if fAPIKey == "" {
		if e := os.Getenv("LINTDEC_API_KEY"); e != "" {
			prov.APIKey = e
		}
	}
	if prov.BaseURL == "" || prov.Model == "" {
		fmt.Fprintln(os.Stderr, "provider incomplete (need baseUrl and model); run: lintdec provider add")
		return 2
	}

	files, err := collectFiles(paths, include, exclude)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	linesCache := map[string][]string{}
	var units []Unit
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: %s: %v (skipped)\n", f, err)
			continue
		}
		u := splitFile(f, b)
		linesCache[f] = strings.Split(string(b), "\n")
		units = append(units, u...)
	}

	var jobs []job
	decisions := 0
	for _, u := range units {
		s := buildJobs(u, active, linesCache[u.File])
		jobs = append(jobs, s...)
		for _, j := range s {
			decisions += len(j.rules)
		}
	}

	if fDumpUnits {
		fmt.Printf("%d files, %d units, %d batches (%d decisions) with rules %v\n", len(files), len(units), len(jobs), decisions, ruleIDList(active))
		for _, u := range units {
			fmt.Printf("  %-12s %s:%d-%d (%d lines)\n", u.Kind, u.File, u.Start, u.End, u.End-u.Start+1)
			if u.End-u.Start+1 > rawBudgetLines && u.Kind != "File" {
				fmt.Printf("    digest:\n")
				for l := range strings.SplitSeq(digestFor(u, linesCache[u.File]), "\n") {
					fmt.Printf("      %s\n", l)
				}
			}
		}
		return 0
	}

	if fRetryErrors {
		b, err := os.ReadFile(filepath.Join(".lintdec", "errored.json"))
		if err != nil || len(b) == 0 {
			fmt.Println("no errored checks to retry")
			return 0
		}
		var keys []string
		if err := json.Unmarshal(b, &keys); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		want := map[string]bool{}
		for _, k := range keys {
			want[k] = true
		}
		var keep []job
		for _, j := range jobs {
			for _, r := range j.rules {
				if want[fmt.Sprintf("%s|%s|%d|%d", r.ID, j.unit.File, j.unit.Start, j.unit.End)] {
					keep = append(keep, j)
					break
				}
			}
		}
		jobs = keep
		os.Remove(filepath.Join(".lintdec", "errored.json"))
		if len(jobs) == 0 {
			fmt.Println("no errored checks match the current tree")
			return 0
		}
	}

	if len(jobs) == 0 {
		fmt.Fprintln(os.Stderr, "0 checks (no unit matches the active rules)")
		return 0
	}
	if !fQuiet {
		fmt.Fprintf(os.Stderr, "lintdec: %d files → %d units, %d batches (%d decisions) · rules: %d · model: %s\n",
			len(files), len(units), len(jobs), decisions, len(active), prov.Model)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cl := &client{
		base:   prov.BaseURL,
		model:  prov.Model,
		apiKey: prov.APIKey,
		temp:   fTemp,
		httpc:  &http.Client{Timeout: 5 * time.Minute},
	}
	ch := newCache(!fNoCache)
	prog := newProgress(len(jobs), fQuiet)

	var ledgerF *os.File
	if fLedger != "" {
		ledgerF, err = os.OpenFile(fLedger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		defer ledgerF.Close()
	}

	var (
		mu            sync.Mutex
		findings      []Finding
		ignoredCount  int
		errs          int
		erroredKeys   []string
		budgetSkipped int
		callsUsed     atomic.Int64
	)

	runJob := func(j job) {
		start := time.Now()
		// per-(unit, rule) cache first: only genuinely uncached rules go to
		// the model, so any --rules combination reuses prior work
		tempStr := fmt.Sprintf("%v", cl.temp)
		verdicts := make(map[string]Verdict, len(j.rules))
		var todo []Rule
		for _, r := range j.rules {
			if v, ok := ch.get(ch.key(cl.model, tempStr, r.ID, j.unit)); ok {
				verdicts[r.ID] = v
				continue
			}
			todo = append(todo, r)
		}
		wasCached := len(todo) == 0
		if len(todo) > 0 {
			if fBudget > 0 && callsUsed.Add(1) > int64(fBudget) {
				mu.Lock()
				budgetSkipped++
				mu.Unlock()
				prog.tick(0, false, "budget")
				return
			}
			v, err := runWithRetry(cl, job{unit: j.unit, rules: todo})
			if err != nil {
				mu.Lock()
				errs++
				for _, r := range todo {
					erroredKeys = append(erroredKeys, fmt.Sprintf("%s|%s|%d|%d", r.ID, j.unit.File, j.unit.Start, j.unit.End))
				}
				show := errs <= 5
				mu.Unlock()
				if show {
					fmt.Fprintf(os.Stderr, "warn: %s: %v\n", jobKey(j), err)
				}
				prog.tick(time.Since(start), false, jobKey(j)+"  ERROR "+err.Error())
				return
			}
			for id, vv := range v {
				ch.put(ch.key(cl.model, tempStr, id, j.unit), vv)
				verdicts[id] = vv
			}
		}
		for _, r := range j.rules {
			vv := verdicts[r.ID]
			if !vv.Lint {
				continue
			}
			// Structural guard, not a size judgment: a hard wrap is where line
			// breaks fall mid-sentence, so a single-line unit or a block of
			// complete sentences (which is what reflowed prose looks like) is
			// not one. Everything else stays with the model.
			if r.ID == "md-hard-wrap" && (strings.Count(j.unit.Text, "\n") < 1 || wrapBreaks(j.unit.Text) < 2) {
				continue
			}
			f := buildFinding(j, r, vv, linesCache)
			if !fNoIgnore && isIgnored(r.ID, j, f.Line, linesCache) {
				mu.Lock()
				ignoredCount++
				mu.Unlock()
				continue
			}
			mu.Lock()
			findings = append(findings, f)
			mu.Unlock()
			if ledgerF != nil {
				b, _ := json.Marshal(f)
				ledgerF.Write(append(b, '\n'))
			}
			prog.addFinding(f)
		}
		label := fmt.Sprintf("%d flagged of %d", lintCount(verdicts), len(j.rules))
		prog.tick(time.Since(start), wasCached, jobKey(j)+" → "+label)
	}

	jobsCh := make(chan job, 64)
	var wg sync.WaitGroup
	nW := max(min(fWorkers, len(jobs)), 1)
	for range nW {
		wg.Go(func() {
			for j := range jobsCh {
				if ctx.Err() != nil {
					return
				}
				runJob(j)
			}
		})
	}
	interrupted := false
	for _, j := range jobs {
		if ctx.Err() != nil {
			interrupted = true
			break
		}
		prog.noteSubmit()
		jobsCh <- j
	}
	close(jobsCh)
	wg.Wait()
	// jobs drain into the channel faster than workers consume them, so the
	// send loop often finishes before SIGINT lands; the context is the truth
	if ctx.Err() != nil {
		interrupted = true
	}

	writeErrored(erroredKeys)

	minRank := severityRank(fSeverity)
	var report []Finding
	sevSuppressed := 0
	for _, f := range findings {
		if severityRank(f.Severity) >= minRank {
			report = append(report, f)
		} else {
			sevSuppressed++
		}
	}
	sortFindings(report)
	emit(report, fOut)

	exit := 0
	if len(report) > 0 {
		exit = 1
	}
	if len(jobs) > 0 && errs == len(jobs) {
		exit = 2
	}
	if interrupted {
		exit = 130
	}
	prog.finish(len(report), errs, ignoredCount, budgetSkipped, sevSuppressed, exit)
	return exit
}

func ruleIDList(rs []Rule) []string {
	ids := make([]string, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	return ids
}

func lintCount(v map[string]Verdict) int {
	n := 0
	for _, x := range v {
		if x.Lint {
			n++
		}
	}
	return n
}

// wrapBreaks counts lines ending mid-sentence: the structural signature
// of hard-wrapping, enforced because the model misreads blocks of complete
// similar-length sentences as wrapped.
func wrapBreaks(text string) int {
	n := 0
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines)-1; i++ {
		t := strings.TrimRight(lines[i], " \t")
		if t == "" {
			continue
		}
		r := []rune(t)
		last := r[len(r)-1]
		if strings.ContainsRune(".!?:…)\"'`*}_", last) {
			continue
		}
		n++
	}
	return n
}

func buildFinding(j job, r Rule, v Verdict, lines map[string][]string) Finding {
	// Line anchors come from the model's guess and are unreliable; findings
	// anchor to the unit start (plan standing rule: real unit spans).
	ln := &j.unit.Start
	line := *ln
	quote := ""
	if ls, ok := lines[j.unit.File]; ok && line >= 1 && line <= len(ls) {
		quote = strings.TrimSpace(ls[line-1])
		if len(quote) > 120 {
			quote = quote[:120] + "…"
		}
	}
	return Finding{Rule: r.ID, Severity: v.Severity, Confidence: v.Confidence, File: j.unit.File, Line: line, Quote: quote, Message: v.Reason}
}

// isIgnored reports whether a //lintdec:ignore (or <!-- lintdec:ignore -->)
// directive within 2 lines above the finding suppresses this rule.
func isIgnored(ruleID string, j job, line int, lines map[string][]string) bool {
	ls, ok := lines[j.unit.File]
	if !ok {
		return false
	}
	lo := max(1, line-2)
	for i := lo; i <= line && i <= len(ls); i++ {
		t := ls[i-1]
		_, rest, found := strings.Cut(t, "lintdec:ignore")
		if !found {
			continue
		}
		ids := cleanIDs(rest)
		if len(ids) == 0 {
			return true
		}
		return slices.Contains(ids, ruleID)
	}
	return false
}

func cleanIDs(s string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			cur.WriteRune(r)
		} else {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func writeErrored(keys []string) {
	if len(keys) == 0 {
		os.Remove(filepath.Join(".lintdec", "errored.json"))
		return
	}
	if err := os.MkdirAll(".lintdec", 0o755); err != nil {
		return
	}
	b, _ := json.MarshalIndent(keys, "", "  ")
	os.WriteFile(filepath.Join(".lintdec", "errored.json"), b, 0o644)
}
