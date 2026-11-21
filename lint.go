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

// defaultInclude returns globs for every language splitFile understands.
func defaultInclude() []string {
	out := make([]string, 0, len(tsExt)+3)
	out = append(out, "*.go", "*.md", "*.markdown")
	for ext := range tsExt {
		out = append(out, "*"+ext)
	}
	slices.Sort(out)
	return out
}

func walkRoot(root string, fn func(string)) {
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

// collectFiles turns positional args (files, dirs, or glob patterns) into a
// file list. Explicitly named files bypass include/exclude.
func collectFiles(paths, include, exclude []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string, explicit bool) {
		p = filepath.ToSlash(filepath.Clean(p))
		if rel, err := filepath.Rel(".", p); err == nil && !strings.HasPrefix(rel, "..") {
			p = filepath.ToSlash(rel)
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			return
		}
		if !explicit && (matchAnyGlob(exclude, p) || (len(include) > 0 && !matchAnyGlob(include, p))) {
			return
		}
		if st.Size() > 1<<20 {
			fmt.Fprintf(os.Stderr, "warn: %s: over 1MB, skipped\n", p)
			return
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, raw := range paths {
		if st, err := os.Stat(raw); err == nil {
			if !st.IsDir() {
				add(raw, true)
			} else {
				walkRoot(raw, func(p string) { add(p, false) })
			}
			continue
		}
		matched := 0
		walkRoot(".", func(p string) {
			if rel, err := filepath.Rel(".", p); err == nil && matchGlob(raw, filepath.ToSlash(rel)) {
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

type lintOpts struct {
	rules, severity, out, provider, baseURL, model, apiKey, include, exclude, ledger string
	workers, budget                                                                  int
	temp                                                                             float64
	noCache, noIgnore, quiet, retryErrors, dumpUnits                                 bool
	setFlags                                                                         map[string]bool
	paths                                                                            []string
}

func parseLintFlags(args []string) (lintOpts, error) {
	var o lintOpts
	fl := flag.NewFlagSet("lint", flag.ContinueOnError)
	fl.StringVar(&o.rules, "rules", "", "comma-separated rule ids (default all)")
	fl.StringVar(&o.severity, "severity", "", "min severity to report: low|med|high")
	fl.StringVar(&o.out, "out", "text", "output: text|json|jsonl")
	fl.StringVar(&o.provider, "provider", "", "provider name from the global config")
	fl.StringVar(&o.baseURL, "base-url", "", "OpenAI-compatible base URL")
	fl.StringVar(&o.model, "model", "", "model id")
	fl.StringVar(&o.apiKey, "api-key", "", "API key")
	fl.IntVar(&o.workers, "workers", 8, "concurrent in-flight batches")
	fl.IntVar(&o.budget, "budget", 0, "max model calls this run, 0 = unlimited")
	fl.Float64Var(&o.temp, "temperature", 0, "sampling temperature")
	fl.BoolVar(&o.noCache, "no-cache", false, "bypass ~/.cache/lintdec/cache")
	fl.BoolVar(&o.noIgnore, "no-ignore", false, "report findings despite inline //lintdec:ignore")
	fl.BoolVar(&o.quiet, "quiet", false, "no progress output")
	fl.StringVar(&o.ledger, "ledger", "", "append findings as JSONL to FILE as decided")
	fl.BoolVar(&o.retryErrors, "retry-errors", false, "re-run only checks that errored last run")
	fl.BoolVar(&o.dumpUnits, "dump-units", false, "print split units and exit (no model calls)")
	fl.StringVar(&o.include, "include", "", "comma-separated glob include filters")
	fl.StringVar(&o.exclude, "exclude", "", "comma-separated glob exclude filters")
	if err := fl.Parse(reorderArgs(args)); err != nil {
		return o, err
	}
	o.setFlags = map[string]bool{}
	fl.Visit(func(f *flag.Flag) { o.setFlags[f.Name] = true })
	o.paths = fl.Args()
	if len(o.paths) == 0 {
		o.paths = []string{"."}
	}
	return o, nil
}

func resolveRules(fRules string, pc ProjectConfig) ([]Rule, error) {
	if fRules != "" {
		var want []Rule
		for id := range strings.SplitSeq(fRules, ",") {
			r, ok := findRule(strings.TrimSpace(id))
			if !ok {
				return nil, fmt.Errorf("unknown rule: %s", id)
			}
			want = append(want, r)
		}
		return want, nil
	}
	active := slices.Clone(rules)
	if len(pc.RulesOn) > 0 {
		active = slices.DeleteFunc(active, func(r Rule) bool { return !slices.Contains(pc.RulesOn, r.ID) })
	}
	return slices.DeleteFunc(active, func(r Rule) bool { return slices.Contains(pc.RulesOff, r.ID) }), nil
}

func resolveProvider(o lintOpts, pc ProjectConfig) (Provider, error) {
	var prov Provider
	if o.baseURL != "" {
		prov = Provider{BaseURL: strings.TrimRight(o.baseURL, "/"), Model: o.model, APIKey: o.apiKey}
	} else {
		name := firstNonEmpty(o.provider, os.Getenv("LINTDEC_PROVIDER"), pc.Provider)
		cfg, err := loadConfig()
		if err != nil {
			return prov, err
		}
		name = firstNonEmpty(name, cfg.Active)
		if name == "" {
			return prov, fmt.Errorf("no provider configured; run: lintdec provider add")
		}
		p, ok := cfg.Providers[name]
		if !ok {
			return prov, fmt.Errorf("no such provider: %s", name)
		}
		prov = p
	}
	if e := os.Getenv("LINTDEC_BASE_URL"); e != "" && o.baseURL == "" {
		prov.BaseURL = strings.TrimRight(e, "/")
	}
	if o.model == "" {
		prov.Model = firstNonEmpty(os.Getenv("LINTDEC_MODEL"), pc.Model, prov.Model)
	}
	if o.apiKey == "" {
		prov.APIKey = firstNonEmpty(os.Getenv("LINTDEC_API_KEY"), prov.APIKey)
	}
	if prov.BaseURL == "" || prov.Model == "" {
		return prov, fmt.Errorf("provider incomplete (need baseUrl and model); run: lintdec provider add")
	}
	return prov, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func filterRetryJobs(jobs []job) ([]job, bool, error) {
	b, err := os.ReadFile(filepath.Join(".lintdec", "errored.json"))
	if err != nil || len(b) == 0 {
		fmt.Println("no errored checks to retry")
		return nil, true, nil
	}
	var keys []string
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, false, err
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
	os.Remove(filepath.Join(".lintdec", "errored.json"))
	if len(keep) == 0 {
		fmt.Println("no errored checks match the current tree")
		return nil, true, nil
	}
	return keep, false, nil
}

type poolResult struct {
	findings      []Finding
	ignoredCount  int
	errs          int
	erroredKeys   []string
	budgetSkipped int
	interrupted   bool
}

func runPool(ctx context.Context, jobs []job, o lintOpts, cl *client, ch *cache, prog *progress, linesCache map[string][]string, ledgerF *os.File) poolResult {
	var (
		mu        sync.Mutex
		res       poolResult
		callsUsed atomic.Int64
	)
	runJob := func(j job) {
		start := time.Now()
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
			if o.budget > 0 && callsUsed.Add(1) > int64(o.budget) {
				mu.Lock()
				res.budgetSkipped++
				mu.Unlock()
				prog.tick(0, false, "budget")
				return
			}
			v, err := runWithRetry(ctx, cl, job{unit: j.unit, rules: todo})
			if err != nil {
				mu.Lock()
				res.errs++
				for _, r := range todo {
					res.erroredKeys = append(res.erroredKeys, fmt.Sprintf("%s|%s|%d|%d", r.ID, j.unit.File, j.unit.Start, j.unit.End))
				}
				show := res.errs <= 5 && ctx.Err() == nil
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
			// Structural guard, not a size judgment: hard wraps break mid-sentence.
			if r.ID == "md-hard-wrap" && (strings.Count(j.unit.Text, "\n") < 1 || wrapBreaks(j.unit.Text) < 2) {
				continue
			}
			f := buildFinding(j, r, vv, linesCache)
			if !o.noIgnore && isIgnored(r.ID, j, f.Line, linesCache) {
				mu.Lock()
				res.ignoredCount++
				mu.Unlock()
				continue
			}
			mu.Lock()
			res.findings = append(res.findings, f)
			if ledgerF != nil {
				b, _ := json.Marshal(f)
				ledgerF.Write(append(b, '\n'))
			}
			mu.Unlock()
			prog.addFinding(f)
		}
		prog.tick(time.Since(start), wasCached, fmt.Sprintf("%s → %d flagged of %d", jobKey(j), lintCount(verdicts), len(j.rules)))
	}

	jobsCh := make(chan job, 64)
	var wg sync.WaitGroup
	nW := max(min(o.workers, len(jobs)), 1)
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
	for _, j := range jobs {
		if ctx.Err() != nil {
			res.interrupted = true
			break
		}
		prog.noteSubmit()
		jobsCh <- j
	}
	close(jobsCh)
	wg.Wait()
	if ctx.Err() != nil {
		res.interrupted = true
	}
	return res
}

func splitAllFiles(files []string, active []Rule) ([]Unit, []job, int, map[string][]string) {
	linesCache := map[string][]string{}
	var units []Unit
	var jobs []job
	decisions := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: %s: %v (skipped)\n", f, err)
			continue
		}
		u := splitFile(f, b)
		linesCache[f] = strings.Split(string(b), "\n")
		units = append(units, u...)
		for _, unit := range u {
			s := buildJobs(unit, active, linesCache[unit.File])
			jobs = append(jobs, s...)
			for _, j := range s {
				decisions += len(j.rules)
			}
		}
	}
	return units, jobs, decisions, linesCache
}

func emitReport(res poolResult, sev, outFmt string, nJobs int, prog *progress) int {
	writeErrored(res.erroredKeys)
	minRank := severityRank(sev)
	var report []Finding
	sevSuppressed := 0
	for _, f := range res.findings {
		if severityRank(f.Severity) >= minRank {
			report = append(report, f)
		} else {
			sevSuppressed++
		}
	}
	sortFindings(report)
	emit(report, outFmt)
	exit := 0
	if len(report) > 0 {
		exit = 1
	}
	if nJobs > 0 && res.errs == nJobs {
		exit = 2
	}
	if res.interrupted {
		exit = 130
	}
	prog.finish(len(report), res.errs, res.ignoredCount, res.budgetSkipped, sevSuppressed, exit)
	return exit
}

func resolveProjectBase(paths []string) string {
	base := paths[0]
	for _, p := range paths[1:] {
		if a, ok := commonAncestor(base, p); ok {
			base = a
		}
	}
	if st, err := os.Stat(base); err == nil && !st.IsDir() {
		return filepath.Dir(base)
	}
	return base
}

func resolveGlobs(o lintOpts, pc ProjectConfig) (include, exclude []string) {
	include = defaultInclude()
	if o.include != "" {
		include = strings.Split(o.include, ",")
	} else if len(pc.Include) > 0 {
		include = pc.Include
	}
	exclude = pc.Exclude
	if o.exclude != "" {
		exclude = strings.Split(o.exclude, ",")
	}
	return include, exclude
}

func dumpUnits(files []string, units []Unit, jobs []job, decisions int, active []Rule) {
	fmt.Printf("%d files, %d units, %d batches (%d decisions) with rules %v\n", len(files), len(units), len(jobs), decisions, ruleIDList(active))
	for _, u := range units {
		fmt.Printf("  %-12s %s:%d-%d (%d lines)\n", u.Kind, u.File, u.Start, u.End, u.End-u.Start+1)
	}
}

func lintCmd(args []string) int {
	o, err := parseLintFlags(args)
	if err != nil {
		return 2
	}
	pc, pcWarn := loadProjectConfig(resolveProjectBase(o.paths))
	for _, w := range pcWarn {
		fmt.Fprintln(os.Stderr, "warn: "+w)
	}
	sev := firstNonEmpty(o.severity, pc.Severity, "low")
	if severityRank(sev) < 0 {
		fmt.Fprintf(os.Stderr, "bad --severity: %s (low|med|high)\n", sev)
		return 2
	}
	include, exclude := resolveGlobs(o, pc)
	active, err := resolveRules(o.rules, pc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	files, err := collectFiles(o.paths, include, exclude)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	units, jobs, decisions, linesCache := splitAllFiles(files, active)
	if o.dumpUnits {
		dumpUnits(files, units, jobs, decisions, active)
		return 0
	}
	prov, err := resolveProvider(o, pc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if o.retryErrors {
		var done bool
		jobs, done, err = filterRetryJobs(jobs)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if done {
			return 0
		}
	}
	if len(jobs) == 0 {
		fmt.Fprintln(os.Stderr, "0 checks (no unit matches the active rules)")
		return 0
	}
	if !o.quiet {
		fmt.Fprintf(os.Stderr, "lintdec: %d files → %d units, %d batches (%d decisions) · rules: %d · model: %s\n",
			len(files), len(units), len(jobs), decisions, len(active), prov.Model)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cl := &client{base: prov.BaseURL, model: prov.Model, apiKey: prov.APIKey, temp: o.temp, httpc: &http.Client{Timeout: 5 * time.Minute}}
	ch := newCache(!o.noCache)
	prog := newProgress(len(jobs), o.quiet)
	var ledgerF *os.File
	if o.ledger != "" {
		ledgerF, err = os.OpenFile(o.ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		defer ledgerF.Close()
	}
	res := runPool(ctx, jobs, o, cl, ch, prog, linesCache, ledgerF)
	return emitReport(res, sev, o.out, len(jobs), prog)
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
