package main

import (
	"context"
	"encoding/json"
	"errors"
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
		"protocol": true, "threshold": true,
		"model": true, "api-key": true, "workers": true, "budget": true, "temperature": true,
		"ledger": true, "include": true, "exclude": true, "diff": true,
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

// commonAncestor returns the longest shared directory prefix of a and b, or (".", false) if none.
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
	ignored := gitIgnored(root)
	filepath.WalkDir(root, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != root && ignored[p] {
			if d.IsDir() {
				fmt.Fprintf(os.Stderr, "skip: %s/ (git-ignored)\n", filepath.ToSlash(p))
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirs[filepath.Base(p)] {
				if d.Name() != ".git" {
					fmt.Fprintf(os.Stderr, "skip: %s/ (always skipped)\n", filepath.ToSlash(p))
				}
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

// gitIgnored: untracked paths under root that git ignores, keyed like WalkDir paths.
// Outside a git checkout it is empty, so only skipDirs apply.
func gitIgnored(root string) map[string]bool {
	out, err := gitOutput(root, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return nil
	}
	ignored := map[string]bool{}
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			ignored[filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(name, "/")))] = true
		}
	}
	return ignored
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
		if ruleApplies(r, u.Kind) && ruleEligible(r, u) {
			app = append(app, r)
		}
	}
	if len(app) == 0 {
		return nil
	}
	if u.Kind == "File" || u.Kind == "Section" || u.Kind == "Document" || u.End-u.Start+1 <= rawBudgetLines {
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
	rules, severity, out, provider, baseURL, protocol, model, apiKey, include, exclude, ledger, diff string
	workers, budget                                                                                  int
	temp, threshold                                                                                  float64
	noCache, noIgnore, quiet, retryErrors, dumpUnits, calibrate                                      bool
	setFlags                                                                                         map[string]bool
	paths                                                                                            []string
}

func parseLintFlags(args []string) (lintOpts, error) {
	var o lintOpts
	fl := flag.NewFlagSet("lint", flag.ContinueOnError)
	fl.StringVar(&o.rules, "rules", "", "comma-separated rule ids (default all)")
	fl.StringVar(&o.severity, "severity", "", "min severity to report: info|warning|error")
	fl.StringVar(&o.out, "out", "text", "output: text|json|jsonl")
	fl.StringVar(&o.provider, "provider", "", "provider name from the global config")
	fl.StringVar(&o.baseURL, "base-url", "", "API base URL")
	fl.StringVar(&o.protocol, "protocol", "", "provider protocol: openai|systemone")
	fl.StringVar(&o.model, "model", "", "model id")
	fl.StringVar(&o.apiKey, "api-key", "", "API key")
	fl.IntVar(&o.workers, "workers", 8, "concurrent in-flight batches")
	fl.IntVar(&o.budget, "budget", 0, "max model calls this run, 0 = unlimited")
	fl.Float64Var(&o.temp, "temperature", 0, "sampling temperature")
	fl.Float64Var(&o.threshold, "threshold", 0.5, "minimum confidence to report (overrides per-rule floors while keeping vetoes)")
	fl.BoolVar(&o.noCache, "no-cache", false, "bypass ~/.cache/lintdec/cache")
	fl.BoolVar(&o.noIgnore, "no-ignore", false, "report findings despite inline //lintdec:ignore")
	fl.BoolVar(&o.quiet, "quiet", false, "no progress output")
	fl.BoolVar(&o.calibrate, "calibrate", false, "report every verdict over --threshold, ignoring rule floors and exceptions (for labeling)")
	fl.StringVar(&o.ledger, "ledger", "", "append findings as JSONL to FILE as decided")
	fl.BoolVar(&o.retryErrors, "retry-errors", false, "re-run only checks that errored last run")
	fl.BoolVar(&o.dumpUnits, "dump-units", false, "print split units and exit (no model calls)")
	fl.StringVar(&o.include, "include", "", "comma-separated glob include filters")
	fl.StringVar(&o.exclude, "exclude", "", "comma-separated glob exclude filters")
	fl.StringVar(&o.diff, "diff", "", "lint units changed since the merge base with this Git ref")
	if err := fl.Parse(reorderArgs(args)); err != nil {
		return o, err
	}
	o.setFlags = map[string]bool{}
	fl.Visit(func(f *flag.Flag) { o.setFlags[f.Name] = true })
	if o.budget < 0 {
		return o, fmt.Errorf("--budget cannot be negative")
	}
	if o.setFlags["threshold"] && !(o.threshold > 0 && o.threshold <= 1) {
		return o, fmt.Errorf("--threshold must be in (0, 1]")
	}
	if o.setFlags["diff"] && o.diff == "" {
		return o, fmt.Errorf("--diff needs a non-empty base ref")
	}
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
	// lintdec.yaml edits the default set: on: adds a rule, off: removes one.
	for _, id := range slices.Concat(pc.RulesOn, pc.RulesOff) {
		if _, ok := findRule(id); !ok {
			fmt.Fprintf(os.Stderr, "warn: lintdec.yaml: unknown rule %s (ignored)\n", id)
		}
	}
	active := slices.DeleteFunc(slices.Clone(rules), func(r Rule) bool { return defaultOff[r.ID] && !slices.Contains(pc.RulesOn, r.ID) })
	return slices.DeleteFunc(active, func(r Rule) bool { return slices.Contains(pc.RulesOff, r.ID) }), nil
}

type endpoint struct {
	BaseURL  string
	Protocol string
	Model    string
	APIKey   string
	Auth     string
	source   authSource
}

func pickProvider(cfg Config, wantProv, wantModel string) (string, Provider, error) {
	if wantProv != "" {
		p, ok := cfg.Providers[wantProv]
		if !ok {
			return "", Provider{}, fmt.Errorf("no such provider: %s", wantProv)
		}
		return wantProv, p, nil
	}
	if wantModel != "" {
		if owner, op, ok := cfg.findProviderForModel(wantModel); ok {
			return owner, op, nil
		}
	}
	if cfg.Active == "" {
		return "", Provider{}, fmt.Errorf("no provider configured; run: lintdec provider add")
	}
	p, ok := cfg.Providers[cfg.Active]
	if !ok {
		return "", Provider{}, fmt.Errorf("no such provider: %s", cfg.Active)
	}
	return cfg.Active, p, nil
}

func resolveProvider(o lintOpts, pc ProjectConfig) (endpoint, error) {
	wantModel := firstNonEmpty(o.model, os.Getenv("LINTDEC_MODEL"), pc.Model)
	wantProv := firstNonEmpty(o.provider, os.Getenv("LINTDEC_PROVIDER"), pc.Provider)
	var ep endpoint
	var selected Provider
	if o.baseURL != "" {
		ep = endpoint{BaseURL: strings.TrimRight(o.baseURL, "/"), Protocol: o.protocol, Model: wantModel, APIKey: o.apiKey}
	} else {
		cfg, err := loadConfig()
		if err != nil {
			return ep, err
		}
		name, p, err := pickProvider(cfg, wantProv, wantModel)
		if err != nil {
			return ep, err
		}
		selected = p
		ep = endpoint{
			BaseURL:  strings.TrimRight(p.BaseURL, "/"),
			Protocol: firstNonEmpty(o.protocol, p.Protocol),
			Model:    firstNonEmpty(wantModel, cfg.activeModelFor(name, p)),
			APIKey:   firstNonEmpty(o.apiKey, p.APIKey),
		}
	}
	if e := os.Getenv("LINTDEC_BASE_URL"); e != "" && o.baseURL == "" {
		ep.BaseURL = strings.TrimRight(e, "/")
	}
	if o.apiKey == "" {
		ep.APIKey = firstNonEmpty(os.Getenv("LINTDEC_API_KEY"), ep.APIKey)
	}
	if ep.BaseURL == "" || ep.Model == "" {
		return ep, fmt.Errorf("provider incomplete (need baseUrl and model); run: lintdec model")
	}
	selected.BaseURL, selected.APIKey, selected.Protocol = ep.BaseURL, ep.APIKey, ep.Protocol
	ep.Auth = selected.Auth
	var err error
	ep.source, err = providerAuth(selected)
	if err != nil {
		return ep, err
	}
	return ep, nil
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
	if os.IsNotExist(err) {
		fmt.Println("no errored checks to retry")
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(b) == 0 {
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
	fatalErr      error
}

func runPool(ctx context.Context, jobs []job, o lintOpts, cl *client, ch *cache, prog *progress, linesCache map[string][]string, ledgerF *os.File) poolResult {
	if cl.budget == nil {
		cl.budget = &requestBudget{limit: int64(o.budget)}
	}
	poolCtx, cancelPool := context.WithCancel(ctx)
	defer cancelPool()
	var (
		mu         sync.Mutex
		res        poolResult
		ledgerWarn atomic.Bool // first failed ledger write is logged once
	)
	recordErr := func(j job, todo []Rule, verdicts map[string]Verdict, err error) {
		mu.Lock()
		res.errs++
		for _, r := range todo {
			if _, ok := verdicts[r.ID]; !ok {
				res.erroredKeys = append(res.erroredKeys, fmt.Sprintf("%s|%s|%d|%d", r.ID, j.unit.File, j.unit.Start, j.unit.End))
			}
		}
		if isFatalHTTP(err) {
			first := res.fatalErr == nil
			if first {
				res.fatalErr = err
			}
			mu.Unlock()
			if first {
				cancelPool()
				prog.warn(fmt.Sprintf("fatal: %v (stopping remaining checks; re-run to resume from cache)", err))
			}
			return
		}
		show := res.errs <= 5 && poolCtx.Err() == nil
		mu.Unlock()
		if show {
			prog.warn(fmt.Sprintf("warn: %s: %v", jobKey(j), err))
		}
	}
	runJob := func(j job) {
		prog.beginFile(j.unit.File)
		defer prog.endFile(j.unit.File)
		start := time.Now()
		tempStr := cl.cacheTemp()
		verdicts := make(map[string]Verdict, len(j.rules))
		var todo []Rule
		for _, r := range j.rules {
			if v, ok := ch.get(ch.key(cl.cacheModel(), tempStr, r.ID, cl.cacheRuleVersion(r.ID), j.unit)); ok {
				if cl.protocol == "systemone" {
					v = applySystemOneThreshold(v, cl.threshold)
				}
				verdicts[r.ID] = v
				continue
			}
			todo = append(todo, r)
		}
		wasCached := len(todo) == 0
		if wasCached && cl.costs != nil {
			cl.costs.recordCachedJob()
		}
		errored := false
		if len(todo) > 0 {
			v, err := runWithRetry(poolCtx, cl, job{unit: j.unit, rules: todo})
			for id, vv := range v {
				ch.put(ch.key(cl.cacheModel(), tempStr, id, cl.cacheRuleVersion(id), j.unit), vv)
				verdicts[id] = vv
			}
			if errors.Is(err, errBudgetExhausted) {
				mu.Lock()
				res.budgetSkipped++
				mu.Unlock()
				prog.tick(0, false, "budget")
				return
			}
			if err != nil {
				errored = true
				recordErr(j, todo, verdicts, err)
			}
		}
		for _, r := range j.rules {
			vv, ok := verdicts[r.ID]
			if !ok || !reportable(r, vv, cl.floorFor(r), cl.calibrate) {
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
				if _, werr := ledgerF.Write(append(b, '\n')); werr != nil && ledgerWarn.CompareAndSwap(false, true) {
					prog.warn(fmt.Sprintf("warn: ledger write failed: %v", werr))
				}
			}
			mu.Unlock()
			prog.addFinding(f)
		}
		if errored {
			prog.tick(time.Since(start), false, fmt.Sprintf("%s:%d · error", j.unit.File, j.unit.Start))
			return
		}
		prog.tick(time.Since(start), wasCached, fmt.Sprintf("%s:%d · %d flagged", j.unit.File, j.unit.Start, lintCount(verdicts)))
	}

	jobsCh := make(chan job, 64)
	var wg sync.WaitGroup
	nW := max(min(o.workers, len(jobs)), 1)
	for range nW {
		wg.Go(func() {
			for j := range jobsCh {
				if poolCtx.Err() != nil {
					return
				}
				runJob(j)
			}
		})
	}
submit:
	for _, j := range jobs {
		select {
		case <-poolCtx.Done():
			break submit
		case jobsCh <- j:
			prog.noteSubmit()
		}
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
		u := slices.DeleteFunc(splitFile(f, b), structuralComment)
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

func emitReport(res poolResult, sev, outFmt string, nJobs int, prog *progress, diff bool) int {
	return emitReportWithTemp(res, sev, outFmt, nJobs, prog, diff, func() (reportFile, error) {
		return os.CreateTemp("", "lintdec-report-*.txt")
	})
}

func emitReportWithTemp(res poolResult, sev, outFmt string, nJobs int, prog *progress, diff bool, createTemp func() (reportFile, error)) int {
	writeErrored(res.erroredKeys)
	minRank := severityRank(sev)
	var report []Finding
	if diff && outFmt == "json" {
		report = []Finding{}
	}
	sevSuppressed := 0
	for _, f := range res.findings {
		if severityRank(f.Severity) >= minRank {
			report = append(report, f)
		} else {
			sevSuppressed++
		}
	}
	report = collapsePerFile(report)
	sortFindings(report)
	exit := 0
	if len(report) > 0 {
		exit = 1
	}
	if (nJobs > 0 && res.errs == nJobs) || res.budgetSkipped > 0 || res.fatalErr != nil {
		exit = 2
	}
	if res.interrupted {
		exit = 130
	}
	prog.clear()
	if interactiveResults(outFmt, prog) {
		prog.resultsHeader(os.Stdout, report, res.errs, res.ignoredCount, res.budgetSkipped, sevSuppressed, exit)
		emitResults(os.Stdout, os.Stderr, report, colorEnabled(true), createTemp)
		prog.resultsCost()
	} else {
		emit(report, outFmt)
		prog.finish(len(report), res.errs, res.ignoredCount, res.budgetSkipped, sevSuppressed, exit)
	}
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

// lintSettings merges flags with the repository's lintdec.yaml: severity and active rules.
func lintSettings(o lintOpts) (ProjectConfig, string, []Rule, error) {
	pc, pcWarn := loadProjectConfig(resolveProjectBase(o.paths))
	for _, w := range pcWarn {
		fmt.Fprintln(os.Stderr, "warn: "+w)
	}
	sev := firstNonEmpty(o.severity, pc.Severity, "info")
	if severityRank(sev) < 0 {
		return pc, "", nil, fmt.Errorf("bad --severity: %s (info|warning|error)", sev)
	}
	active, err := resolveRules(o.rules, pc)
	return pc, sev, active, err
}

func lintCmd(args []string) int {
	o, err := parseLintFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	pc, sev, active, err := lintSettings(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	include, exclude := resolveGlobs(o, pc)
	files, changes, err := lintTargets(o, include, exclude)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if changes != nil && len(files) == 0 {
		return nothingToLint(o, "no changed files to lint")
	}
	units, jobs, decisions, linesCache := splitAllFiles(files, active)
	if changes != nil {
		units, jobs, decisions = changedUnits(units, changes, active, linesCache)
	}
	if o.dumpUnits {
		dumpUnits(files, units, jobs, decisions, active)
		return 0
	}
	if len(jobs) == 0 {
		return nothingToLint(o, "0 checks (no unit matches the active rules)")
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cl := newLintClient(prov, o)
	ch := newCache(!o.noCache)
	if !o.quiet {
		printLintHeader(len(files), len(units), jobs, len(active), cl.model)
	}
	return runLint(ctx, o, jobs, cl, ch, linesCache, sev, changes != nil)
}

// nothingToLint ends a run with no checks; JSON callers still get an empty array.
func nothingToLint(o lintOpts, msg string) int {
	if o.out == "json" {
		emit([]Finding{}, o.out)
	}
	fmt.Fprintln(os.Stderr, msg)
	return 0
}

func runLint(ctx context.Context, o lintOpts, jobs []job, cl *client, ch *cache, linesCache map[string][]string, sev string, diff bool) int {
	prog := newProgress(len(jobs), o.quiet)
	prog.costs = cl.costs
	var ledgerF *os.File
	if o.ledger != "" {
		var err error
		ledgerF, err = os.OpenFile(o.ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		defer ledgerF.Close()
	}
	if prog.tty && !prog.quiet {
		prog.drawBar()
	}
	res := runPool(ctx, jobs, o, cl, ch, prog, linesCache, ledgerF)
	return emitReport(res, sev, o.out, len(jobs), prog, diff)
}

// lintTargets: the files to lint, plus changed ranges in diff mode.
func lintTargets(o lintOpts, include, exclude []string) ([]string, map[string]diffRanges, error) {
	if o.diff == "" {
		files, err := collectFiles(o.paths, include, exclude)
		return files, nil, err
	}
	changes, err := collectDiff(".", o.diff)
	if err != nil {
		return nil, nil, err
	}
	files, err := diffFiles(changes, o.paths, include, exclude)
	return files, changes, err
}

// changedUnits keeps units a diff touches, including a changed doc comment above them.
func changedUnits(units []Unit, changes map[string]diffRanges, active []Rule, linesCache map[string][]string) ([]Unit, []job, int) {
	changed := func(u Unit) bool {
		path, err := canonicalDiffPath(u.File)
		return err == nil && (changes[path].intersects(u.Start, u.End) || (u.ContextStart > 0 && changes[path].intersects(u.ContextStart, u.Start-1)))
	}
	units = slices.DeleteFunc(units, func(u Unit) bool { return !changed(u) })
	var jobs []job
	decisions := 0
	for _, u := range units {
		for _, j := range buildJobs(u, active, linesCache[u.File]) {
			jobs = append(jobs, j)
			decisions += len(j.rules)
		}
	}
	return units, jobs, decisions
}

func newLintClient(prov endpoint, o lintOpts) *client {
	return &client{
		base:         prov.BaseURL,
		protocol:     prov.Protocol,
		model:        prov.Model,
		apiKey:       prov.APIKey,
		source:       prov.source,
		trustCost:    reportsProviderCost(prov.BaseURL),
		costs:        &costAccumulator{},
		budget:       &requestBudget{limit: int64(o.budget)},
		temp:         o.temp,
		threshold:    o.threshold,
		thresholdSet: o.setFlags["threshold"],
		calibrate:    o.calibrate,
		httpc:        &http.Client{Timeout: 5 * time.Minute},
	}
}

// printLintHeader writes the run summary to stderr so stdout stays reserved for findings.
func printLintHeader(nFiles, nUnits int, jobs []job, nRules int, model string) {
	decisions := 0
	for _, j := range jobs {
		decisions += len(j.rules)
	}
	if terminal(os.Stderr) {
		color := colorEnabled(true)
		fmt.Fprintf(os.Stderr, "\n  %s  %s\n", paint("LINTDEC", accent, color), paint("Checking your files", bold, color))
		fmt.Fprintf(os.Stderr, "  %s\n", paint(fmt.Sprintf("%d files  ·  %d units  ·  %d checks  ·  %d rules  ·  %s", nFiles, nUnits, len(jobs), nRules, cleanText(model)), dim, color))
		fmt.Fprintln(os.Stderr)
		return
	}
	fmt.Fprintf(os.Stderr, "lintdec: %d files → %d units, %d batches (%d decisions) · rules: %d · model: %s\n",
		nFiles, nUnits, len(jobs), decisions, nRules, model)
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

func buildFinding(j job, r Rule, v Verdict, lines map[string][]string) Finding {
	// Model-provided line numbers are unreliable, so anchor to the unit start.
	ln := &j.unit.Start
	line := *ln
	quote := ""
	if spanKinds[j.unit.Kind] {
		quote = j.unit.Text
		// keep the marked word in view when the sentence is longer than the quote
		if k := strings.Index(quote, "⟦"); k > 160 {
			quote = "…" + quote[k-120:]
		}
	} else if ls, ok := lines[j.unit.File]; ok && line >= 1 && line <= len(ls) {
		quote = strings.TrimSpace(ls[line-1])
	}
	if len(quote) > 240 {
		quote = quote[:240] + "…"
	}
	span := ""
	if a, b := strings.Index(j.unit.Text, "⟦"), strings.Index(j.unit.Text, "⟧"); spanKinds[j.unit.Kind] && a >= 0 && b > a {
		span = j.unit.Text[a+len("⟦") : b]
	}
	return Finding{Rule: r.ID, Severity: ruleLevel[r.ID], Confidence: v.Confidence, File: j.unit.File, Line: line, EndLine: j.unit.End,
		Span: span, Quote: quote, Message: v.Reason, Veto: v.Veto, Source: r.Source}
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
