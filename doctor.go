package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type DoctorFinding struct {
	Stack    string `json:"stack"`
	ID       string `json:"id"`
	Severity string `json:"severity"` // "error" | "warning"
	File     string `json:"file,omitempty"`
	Message  string `json:"message"`
	FixHint  string `json:"fix_hint"`
}

type DoctorReport struct {
	Root         string              `json:"root"`
	Stacks       []string            `json:"stacks"`
	Configs      map[string][]string `json:"configs"`
	Workflows    []string            `json:"workflows,omitempty"`
	Suppressions map[string]int      `json:"suppressions,omitempty"`
	Findings     []DoctorFinding     `json:"findings"`
	Fix          *DoctorFixResult    `json:"fix,omitempty"`
}

type doctorOpts struct {
	root        string
	out         string
	exclude     string
	fix         bool
	dryRun      bool
	provider    string
	baseURL     string
	protocol    string
	model       string
	apiKey      string
	temperature float64
}

type repoInventory struct {
	root         string
	hasTS        bool
	tsManifest   bool
	goManifest   bool
	pyManifest   bool
	tsCount      int
	goCount      int
	pyCount      int
	shellCount   int
	configs      map[string][]string
	manifests    map[string]bool
	srcDirs      map[string]bool
	workflows    []string
	suppressions map[string]int
}

var (
	doctorSuppTSRe   = regexp.MustCompile(`(?i)(?:eslint-disable|@ts-ignore|@ts-expect-error|@ts-nocheck|oxlint-disable|biome-ignore)`)
	doctorSuppGoRe   = regexp.MustCompile(`//\s*nolint\b`)
	doctorSuppPyRe   = regexp.MustCompile(`(?i)#\s*(?:noqa|ruff:\s*noqa|type:\s*ignore|pyright:\s*ignore|pylint:\s*disable)\b`)
	doctorSuppShRe   = regexp.MustCompile(`(?i)#\s*shellcheck\s+disable=`)
	tsStrictTrueRe   = regexp.MustCompile(`"strict"\s*:\s*true\b`)
	tsUnusedOffRe    = regexp.MustCompile(`reportUnusedDisableDirectives\s*[:=]\s*(?:['"]off['"]|false)`)
	tsRuleAssignRe   = regexp.MustCompile(`(?m)(?:^|[\s,{])['"]?((?:@typescript-eslint/|typescript/)?(?:no-floating-promises|no-misused-promises|no-explicit-any|no-unused-vars|no-empty))['"]?\s*:\s*(?:['"](off|warn|error)['"]|([012])\b|\[\s*['"](off|warn|error)['"])`)
	goCoreLinters    = []string{"errcheck", "govet", "staticcheck", "ineffassign", "gosec", "errorlint", "modernize"}
	goListEntryRe    = regexp.MustCompile(`(?m)^\s*-\s*["']?([a-zA-Z0-9_-]+)["']?\b`)
	goInlineListRe   = regexp.MustCompile(`(?m)["']?(disable|enable)["']?\s*[:=]\s*\[([^\]]*)\]`)
	goDefaultNoneRe  = regexp.MustCompile(`(?i)\b(?:default\s*[:=]\s*["']?none["']?|disable-all\s*[:=]\s*true)\b`)
	goDefaultAllRe   = regexp.MustCompile(`(?i)\b(?:default\s*[:=]\s*["']?all["']?|enable-all\s*[:=]\s*true)\b`)
	goPresetsRe      = regexp.MustCompile(`(?i)(?:^|[\s,{])["']?presets["']?\s*[:=]`)
	goIdentTokenRe   = regexp.MustCompile(`[a-zA-Z0-9_-]+`)
	pyRuffSelectRe   = regexp.MustCompile(`(?m)^\s*(?:extend-)?select\s*=\s*\[([^\]]*)\]`)
	pyRuffIgnoreRe   = regexp.MustCompile(`(?m)^\s*(?:extend-)?ignore\s*=\s*\[([^\]]*)\]`)
	pyRuffWeakIgnRe  = regexp.MustCompile(`(?i)['"](?:F841|F401|E722|B\d*|S\d*|ASYNC\d*)['"]`)
	pyTypeWeakRe     = regexp.MustCompile(`(?i)(?:typeCheckingMode\s*=\s*['"](?:off|basic)['"]|ignore_errors\s*=\s*true|strict\s*=\s*false)`)
	ciNeuteredStepRe = regexp.MustCompile(`(?i)(?:eslint|oxlint|biome|golangci-lint|ruff|mypy|pyright|shellcheck|npm\s+run\s+lint|pnpm\s+(?:run\s+)?lint|yarn\s+lint)[^\n]*(?:\|\|\s*true|\|\|\s*exit\s+0|--max-warnings\s+[1-9])`)
	ciContinueErrRe  = regexp.MustCompile(`(?m)continue-on-error\s*:\s*true`)
	ciLinterCmdRe    = map[string]*regexp.Regexp{
		"ts":     regexp.MustCompile(`(?i)\b(?:eslint|oxlint|biome|tsc\b|sloplint|npm\s+run\s+(?:lint|typecheck|check)|pnpm\s+(?:run\s+)?(?:lint|typecheck|check)|yarn\s+(?:lint|typecheck|check)|bun\s+run\s+(?:lint|typecheck|check))\b`),
		"go":     regexp.MustCompile(`(?i)\b(?:golangci-lint|go\s+vet)\b`),
		"python": regexp.MustCompile(`(?i)\b(?:ruff|mypy|pyright|flake8|pylint|scripts/check|scripts/lint)\b`),
		"shell":  regexp.MustCompile(`(?i)\bshellcheck\b`),
	}
)

func doctorCmd(args []string) int {
	o, err := parseDoctorFlags(args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	pc, warns := loadProjectConfig(o.root)
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "warn:", w)
	}
	excludes := doctorExcludes(o, pc)
	rep, err := runDoctorAuditWithExclude(o.root, excludes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if o.fix && len(rep.Findings) > 0 {
		code := runDoctorFixFlow(o, pc, excludes, &rep)
		if code != 0 {
			return code
		}
	}
	writeDoctorReport(os.Stdout, rep, o.out)
	if len(rep.Findings) > 0 {
		return 1
	}
	return 0
}

func doctorExcludes(o doctorOpts, pc ProjectConfig) []string {
	out := append([]string(nil), pc.Exclude...)
	for _, raw := range strings.Split(o.exclude, ",") {
		if g := strings.TrimSpace(raw); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func runDoctorFixFlow(o doctorOpts, pc ProjectConfig, excludes []string, rep *DoctorReport) int {
	ep, note, err := resolveDoctorChatProvider(o, pc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if note != "" && o.out == "text" {
		fmt.Fprintln(os.Stderr, note)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := applyDoctorFix(ctx, o.root, *rep, ep, o.temperature, o.dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if res.Cost != "" && o.out == "text" {
		fmt.Fprintln(os.Stderr, res.Cost)
	}
	if !o.dryRun {
		after, err := runDoctorAuditWithExclude(o.root, excludes)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		*rep = after
	}
	rep.Fix = &res
	return 0
}

func parseDoctorFlags(args []string) (doctorOpts, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var o doctorOpts
	fs.StringVar(&o.out, "out", "text", "output: text|json")
	fs.StringVar(&o.exclude, "exclude", "", "comma-separated glob exclude filters")
	fs.BoolVar(&o.fix, "fix", false, "generate/harden deterministic linter configs and diff-gated CI using a configured chat LLM")
	fs.BoolVar(&o.dryRun, "dry-run", false, "with --fix, preview proposed files without writing to disk")
	fs.StringVar(&o.provider, "provider", "", "provider name from global config (for --fix)")
	fs.StringVar(&o.baseURL, "base-url", "", "API base URL (for --fix)")
	fs.StringVar(&o.protocol, "protocol", "", "provider protocol: openai (for --fix)")
	fs.StringVar(&o.model, "model", "", "chat model id (for --fix)")
	fs.StringVar(&o.apiKey, "api-key", "", "API key (for --fix)")
	fs.Float64Var(&o.temperature, "temperature", 0, "sampling temperature (for --fix)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: lintdec doctor [flags] [path]

Audits deterministic linter configs, typecheckers, suppression hygiene, and CI wiring.
With --fix, uses a configured chat (openai-protocol) LLM to generate or harden standalone
linter configs and diff-gated CI workflows directly on disk (rely on git diff to review/revert).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return o, err
	}
	if o.out != "text" && o.out != "json" {
		return o, fmt.Errorf("unknown --out %q (want text|json)", o.out)
	}
	if fs.NArg() > 1 {
		return o, fmt.Errorf("doctor takes at most one path argument")
	}
	o.root = "."
	if fs.NArg() == 1 {
		o.root = fs.Arg(0)
	}
	st, err := os.Stat(o.root)
	if err != nil || !st.IsDir() {
		return o, fmt.Errorf("doctor target must be a directory: %s", o.root)
	}
	return o, nil
}

func runDoctorAudit(root string) (DoctorReport, error) {
	return runDoctorAuditWithExclude(root, nil)
}

func runDoctorAuditWithExclude(root string, excludes []string) (DoctorReport, error) {
	inv, err := scanDoctorRepo(root, excludes)
	if err != nil {
		return DoctorReport{}, err
	}
	rep := DoctorReport{
		Root:         filepath.ToSlash(filepath.Clean(root)),
		Configs:      inv.configs,
		Workflows:    inv.workflows,
		Suppressions: inv.suppressions,
	}
	if inv.tsManifest || inv.tsCount >= 3 {
		rep.Stacks = append(rep.Stacks, "ts")
		rep.Findings = append(rep.Findings, auditTS(inv)...)
	}
	if inv.goManifest || inv.goCount >= 3 {
		rep.Stacks = append(rep.Stacks, "go")
		rep.Findings = append(rep.Findings, auditGo(inv)...)
	}
	if inv.pyManifest || inv.pyCount >= 3 {
		rep.Stacks = append(rep.Stacks, "python")
		rep.Findings = append(rep.Findings, auditPython(inv)...)
	}
	if inv.shellCount >= 3 || len(inv.configs["shell"]) > 0 {
		rep.Stacks = append(rep.Stacks, "shell")
		rep.Findings = append(rep.Findings, auditShell(inv)...)
	}
	if len(rep.Stacks) > 0 {
		rep.Findings = append(rep.Findings, auditCI(inv, rep.Stacks)...)
	}
	return rep, nil
}

func scanDoctorRepo(root string, excludes []string) (repoInventory, error) {
	inv := repoInventory{
		root:         root,
		configs:      map[string][]string{},
		manifests:    map[string]bool{},
		srcDirs:      map[string]bool{},
		suppressions: map[string]int{},
	}
	walkRootOpts(root, true, func(p string) {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return
		}
		rel = filepath.ToSlash(rel)
		if matchAnyGlob(excludes, rel) {
			return
		}
		classifyDoctorFile(&inv, p, rel)
	})
	pruneFixtureConfigs(&inv)
	slices.Sort(inv.workflows)
	return inv, nil
}

func configStack(key string) string {
	switch key {
	case "ts", "tsconfig":
		return "ts"
	case "go":
		return "go"
	case "python_lint", "python_type", "pyproject":
		return "python"
	default:
		return key
	}
}

func pruneFixtureConfigs(inv *repoInventory) {
	for k, list := range inv.configs {
		stack := configStack(k)
		var kept []string
		for _, rel := range list {
			if !fixtureScoped(stack, rel, inv.manifests, inv.srcDirs) {
				kept = append(kept, rel)
			}
		}
		if len(kept) == 0 {
			delete(inv.configs, k)
		} else {
			slices.Sort(kept)
			inv.configs[k] = kept
		}
	}
	activateSurvivingStacks(inv)
}

func activateSurvivingStacks(inv *repoInventory) {
	if len(inv.configs["tsconfig"]) > 0 {
		inv.hasTS, inv.tsManifest = true, true
	}
	if len(inv.configs["python_lint"]) > 0 || len(inv.configs["python_type"]) > 0 || len(inv.configs["pyproject"]) > 0 {
		inv.pyManifest = true
	}
	for key := range inv.manifests {
		if !inv.srcDirs[key] {
			continue
		}
		if strings.HasPrefix(key, "ts:") {
			inv.tsManifest = true
		} else if strings.HasPrefix(key, "go:") {
			inv.goManifest = true
		} else if strings.HasPrefix(key, "python:") {
			inv.pyManifest = true
		}
	}
}

// fixtureScoped drops configs under fixture/example dirs unless that subtree has both a stack manifest and sources.
func fixtureScoped(stack, rel string, manifests, srcDirs map[string]bool) bool {
	if !isExampleOrFixturePath(rel) {
		return false
	}
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		key := stack + ":" + dir
		if isExampleOrFixtureDir(dir) && manifests[key] && srcDirs[key] {
			return false
		}
	}
	return true
}

func isExampleOrFixtureDir(dir string) bool {
	dir = strings.ToLower(dir)
	if dir == "." || dir == "/" {
		return false
	}
	for seg := range strings.SplitSeq(dir, "/") {
		switch seg {
		case "testdata", "docs", "examples", "example", "fixtures":
			return true
		}
	}
	return false
}

func isExampleOrFixturePath(rel string) bool {
	return isExampleOrFixtureDir(path.Dir(rel))
}

func markSourceAncestors(srcDirs map[string]bool, stack, dir string) {
	for d := dir; d != "." && d != "/"; d = path.Dir(d) {
		srcDirs[stack+":"+d] = true
	}
}

func classifyDoctorFile(inv *repoInventory, abs, rel string) {
	base := strings.ToLower(path.Base(rel))
	if strings.HasPrefix(rel, ".github/workflows/") && (strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")) {
		inv.workflows = append(inv.workflows, rel)
		return
	}
	recordDoctorConfig(inv, base, rel)
	ext := extOf(rel)
	dir := path.Dir(rel)
	switch ext {
	case ".ts", ".tsx", ".mts", ".cts":
		inv.hasTS = true
		inv.tsCount++
		markSourceAncestors(inv.srcDirs, "ts", dir)
		countFileSuppressions(inv, "ts", abs, doctorSuppTSRe)
	case ".js", ".jsx", ".mjs", ".cjs":
		inv.tsCount++
		markSourceAncestors(inv.srcDirs, "ts", dir)
		countFileSuppressions(inv, "ts", abs, doctorSuppTSRe)
	case ".go":
		inv.goCount++
		markSourceAncestors(inv.srcDirs, "go", dir)
		countFileSuppressions(inv, "go", abs, doctorSuppGoRe)
	case ".py":
		inv.pyCount++
		markSourceAncestors(inv.srcDirs, "python", dir)
		countFileSuppressions(inv, "python", abs, doctorSuppPyRe)
	case ".sh":
		inv.shellCount++
		countFileSuppressions(inv, "shell", abs, doctorSuppShRe)
	case "":
		if src, ok := readSmallFile(abs); ok && isShellShebang(src) {
			inv.shellCount++
			if !isGeneratedFile(src) {
				inv.suppressions["shell"] += len(doctorSuppShRe.FindAllIndex(src, -1))
			}
		}
	}
}

func isTSLinterConfig(base string) bool {
	return strings.HasPrefix(base, "eslint.config.") || strings.HasPrefix(base, ".eslintrc") ||
		strings.HasPrefix(base, ".oxlintrc.") || strings.HasPrefix(base, "oxlint.config.") ||
		base == "biome.json" || base == "biome.jsonc"
}

func isTSConfigFile(base string) bool {
	return strings.HasPrefix(base, "tsconfig") && strings.HasSuffix(base, ".json")
}

func isPythonManifest(base string) bool {
	return base == "pyproject.toml" || base == "setup.py" || base == "setup.cfg" || strings.HasPrefix(base, "requirements")
}

func isPythonTypeConfig(base string) bool {
	return base == "mypy.ini" || base == ".mypy.ini" || base == "pyrightconfig.json"
}

func recordManifest(inv *repoInventory, stack, rel string) {
	inv.manifests[stack+":"+path.Dir(rel)] = true
	if isExampleOrFixturePath(rel) {
		return
	}
	switch stack {
	case "ts":
		inv.tsManifest = true
	case "go":
		inv.goManifest = true
	case "python":
		inv.pyManifest = true
	}
}

func recordDoctorConfig(inv *repoInventory, base, rel string) {
	switch {
	case base == "package.json":
		recordManifest(inv, "ts", rel)
	case isTSConfigFile(base):
		inv.configs["tsconfig"] = append(inv.configs["tsconfig"], rel)
	case isTSLinterConfig(base):
		inv.configs["ts"] = append(inv.configs["ts"], rel)
	case base == "go.mod":
		recordManifest(inv, "go", rel)
	case strings.HasPrefix(base, ".golangci."):
		inv.configs["go"] = append(inv.configs["go"], rel)
	case base == "ruff.toml" || base == ".ruff.toml":
		inv.configs["python_lint"] = append(inv.configs["python_lint"], rel)
	case isPythonManifest(base):
		recordManifest(inv, "python", rel)
		if base == "pyproject.toml" {
			inv.configs["pyproject"] = append(inv.configs["pyproject"], rel)
		}
	case isPythonTypeConfig(base):
		inv.configs["python_type"] = append(inv.configs["python_type"], rel)
	case base == ".shellcheckrc":
		inv.configs["shell"] = append(inv.configs["shell"], rel)
	}
}

func readSmallFile(abs string) ([]byte, bool) {
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() || st.Size() > 1<<20 {
		return nil, false
	}
	b, err := os.ReadFile(abs)
	return b, err == nil
}

func countFileSuppressions(inv *repoInventory, stack, abs string, re *regexp.Regexp) {
	src, ok := readSmallFile(abs)
	if !ok || isGeneratedFile(src) {
		return
	}
	if n := len(re.FindAllIndex(src, -1)); n > 0 {
		inv.suppressions[stack] += n
	}
}

func auditTS(inv repoInventory) []DoctorFinding {
	var out []DoctorFinding
	if len(inv.configs["ts"]) == 0 {
		out = append(out, DoctorFinding{
			Stack: "ts", ID: "ts-missing-linter", Severity: "error",
			Message: "no JS/TS linter config found (ESLint, Oxlint, or Biome)",
			FixHint: "add .oxlintrc.json or eslint.config.mjs with correctness/type-aware rules",
		})
	}
	if inv.hasTS && len(inv.configs["tsconfig"]) > 0 && !anyStrictTSConfig(inv) {
		out = append(out, DoctorFinding{
			Stack: "ts", ID: "ts-tsconfig-not-strict", Severity: "error", File: inv.configs["tsconfig"][0],
			Message: `tsconfig.json does not set "strict": true`,
			FixHint: `enable "strict": true in compilerOptions`,
		})
	}
	for _, rel := range inv.configs["ts"] {
		out = append(out, auditTSConfigFile(inv.root, rel)...)
	}
	if inv.suppressions["ts"] >= 5 && len(inv.configs["ts"]) > 0 && !hasTSReasonEnforcement(inv) {
		out = append(out, DoctorFinding{
			Stack: "ts", ID: "ts-suppression-hygiene", Severity: "warning", File: inv.configs["ts"][0],
			Message: fmt.Sprintf("%d inline TS/JS suppressions found without require-description or unused-disable enforcement", inv.suppressions["ts"]),
			FixHint: "enable eslint-comments/require-description and reportUnusedDisableDirectives",
		})
	}
	return out
}

func auditTSConfigFile(root, rel string) []DoctorFinding {
	src, ok := readSmallFile(filepath.Join(root, filepath.FromSlash(rel)))
	if !ok {
		return nil
	}
	text := string(src)
	var out []DoctorFinding
	if tsUnusedOffRe.MatchString(text) {
		out = append(out, DoctorFinding{
			Stack: "ts", ID: "ts-unused-disables-off", Severity: "warning", File: rel,
			Message: "reportUnusedDisableDirectives is disabled, allowing stale suppressions to accumulate",
			FixHint: `set linterOptions.reportUnusedDisableDirectives to "error"`,
		})
	}
	if weak := findWeakTSCoreRule(text); weak != "" {
		out = append(out, DoctorFinding{
			Stack: "ts", ID: "ts-core-rule-weakened", Severity: "warning", File: rel,
			Message: fmt.Sprintf("core correctness rule weakened (%s)", weak),
			FixHint: "enforce core TS/JS correctness rules at error severity (use diff-gated CI for legacy debt)",
		})
	}
	return out
}

// findWeakTSCoreRule ignores bare JS rules turned off when their TS-scoped equivalent is set to error.
func findWeakTSCoreRule(text string) string {
	matches := tsRuleAssignRe.FindAllStringSubmatch(text, -1)
	scopedErr := map[string]bool{}
	for _, m := range matches {
		name, sev := m[1], firstNonEmpty(m[2], m[3], m[4])
		if base, ok := stripTSRulePrefix(name); ok && (sev == "error" || sev == "2") {
			scopedErr[base] = true
		}
	}
	for _, m := range matches {
		name, sev := m[1], firstNonEmpty(m[2], m[3], m[4])
		if sev == "error" || sev == "2" {
			continue
		}
		if _, isScoped := stripTSRulePrefix(name); !isScoped && scopedErr[name] {
			continue
		}
		return fmt.Sprintf("%s: %s", name, sev)
	}
	return ""
}

func stripTSRulePrefix(name string) (string, bool) {
	if rest, ok := strings.CutPrefix(name, "@typescript-eslint/"); ok {
		return rest, true
	}
	if rest, ok := strings.CutPrefix(name, "typescript/"); ok {
		return rest, true
	}
	return name, false
}

func hasTSReasonEnforcement(inv repoInventory) bool {
	for _, rel := range inv.configs["ts"] {
		if src, ok := readSmallFile(filepath.Join(inv.root, filepath.FromSlash(rel))); ok {
			s := string(src)
			if strings.Contains(s, "require-description") || strings.Contains(s, "reportUnusedDisableDirectives") {
				return true
			}
		}
	}
	return false
}

func anyStrictTSConfig(inv repoInventory) bool {
	for _, rel := range inv.configs["tsconfig"] {
		if src, ok := readSmallFile(filepath.Join(inv.root, filepath.FromSlash(rel))); ok {
			if tsStrictTrueRe.Match(stripJSONComments(src)) {
				return true
			}
		}
	}
	return false
}

func auditGo(inv repoInventory) []DoctorFinding {
	if len(inv.configs["go"]) == 0 {
		return []DoctorFinding{{
			Stack: "go", ID: "go-missing-config", Severity: "error",
			Message: "no .golangci.yml config found",
			FixHint: "add a v2 .golangci.yml enabling errcheck, govet, staticcheck, ineffassign, gosec, errorlint, modernize, and nolintlint",
		}}
	}
	var out []DoctorFinding
	for _, rel := range inv.configs["go"] {
		out = append(out, auditGoConfigFile(inv.root, rel)...)
	}
	return out
}

func auditGoConfigFile(root, rel string) []DoctorFinding {
	src, ok := readSmallFile(filepath.Join(root, filepath.FromSlash(rel)))
	if !ok {
		return nil
	}
	cleaned := stripHashComments(string(src))
	disabled, enabled := parseGoLinterLists(cleaned)
	defaultNone := goDefaultNoneRe.MatchString(cleaned)
	defaultAll := goDefaultAllRe.MatchString(cleaned)
	// Skip inferred disables when presets are used since preset contents vary by version; explicit disable: still fires.
	usesPresets := goPresetsRe.MatchString(cleaned)

	var missingCore []string
	for _, name := range goCoreLinters {
		if disabled[name] || (defaultNone && !enabled[name] && !usesPresets) {
			missingCore = append(missingCore, name)
		}
	}
	var out []DoctorFinding
	if len(missingCore) > 0 {
		out = append(out, DoctorFinding{
			Stack: "go", ID: "go-disabled-core-linters", Severity: "error", File: rel,
			Message: fmt.Sprintf("core Go linters disabled: %s", strings.Join(missingCore, ", ")),
			FixHint: "re-enable core linters and gate CI with golangci-lint run --new-from-merge-base=origin/main",
		})
	}
	nolintOn := (defaultAll && !disabled["nolintlint"]) || enabled["nolintlint"]
	if !nolintOn {
		out = append(out, DoctorFinding{
			Stack: "go", ID: "go-missing-nolintlint", Severity: "warning", File: rel,
			Message: "nolintlint is not enabled to require reasons on //nolint directives",
			FixHint: "enable nolintlint with require-explanation: true and require-specific: true",
		})
	}
	return out
}

func stripHashComments(text string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(text, "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func parseGoLinterLists(cleaned string) (disabled, enabled map[string]bool) {
	disabled, enabled = map[string]bool{}, map[string]bool{}
	for _, m := range goInlineListRe.FindAllStringSubmatch(cleaned, -1) {
		target := enabled
		if m[1] == "disable" {
			target = disabled
		}
		for _, tok := range goIdentTokenRe.FindAllString(m[2], -1) {
			target[tok] = true
		}
	}
	var cur map[string]bool
	curIndent := 0
	for line := range strings.SplitSeq(cleaned, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if trimmed == "disable:" {
			cur, curIndent = disabled, indent
			continue
		}
		if trimmed == "enable:" {
			cur, curIndent = enabled, indent
			continue
		}
		if cur != nil && indent <= curIndent && !strings.HasPrefix(trimmed, "-") {
			cur = nil
		}
		if cur != nil {
			if m := goListEntryRe.FindStringSubmatch(line); m != nil {
				cur[m[1]] = true
			}
		}
	}
	return disabled, enabled
}

func auditPython(inv repoInventory) []DoctorFinding {
	var out []DoctorFinding
	ruffFile, ruffText, hasTypeCfg, typeFile, typeText := readPythonConfigs(inv)
	if ruffFile == "" {
		out = append(out, DoctorFinding{
			Stack: "python", ID: "py-missing-linter", Severity: "error",
			Message: "no Ruff config found (ruff.toml or [tool.ruff] in pyproject.toml)",
			FixHint: `add ruff.toml with select = ["E", "F", "B", "SIM", "FURB", "UP", "RUF100"]`,
		})
	} else if pyRuffNeedsHardening(ruffText) {
		out = append(out, DoctorFinding{
			Stack: "python", ID: "py-ruff-weakened", Severity: "warning", File: ruffFile,
			Message: "Ruff config misses bugbear/modernization rules (B, FURB/SIM/UP, RUF100) or ignores core checks",
			FixHint: `select B, SIM, FURB, UP, RUF100 and avoid globally ignoring F841/F401/E722/ASYNC`,
		})
	}
	if !hasTypeCfg {
		out = append(out, DoctorFinding{
			Stack: "python", ID: "py-missing-typechecker", Severity: "warning",
			Message: "no Python typechecker config found (mypy or pyright)",
			FixHint: `add mypy.ini with strict = true or pyrightconfig.json with typeCheckingMode = "standard"`,
		})
	} else if isPythonTypeConfigWeak(typeText) {
		out = append(out, DoctorFinding{
			Stack: "python", ID: "py-typechecker-weak", Severity: "warning", File: typeFile,
			Message: "Python typechecker is weakened (basic/off mode or strict = false)",
			FixHint: `enable strict = true (mypy) or typeCheckingMode = "standard"/"strict" (pyright)`,
		})
	}
	return out
}

// isPythonTypeConfigWeak ignores per-module mypy override blocks ([mypy-foo.*], [[tool.mypy.overrides]])
// since per-module ignore_errors is mypy's documented baselining mechanism.
func isPythonTypeConfigWeak(text string) bool {
	inScopedOverride := false
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			low := strings.ToLower(trimmed)
			inScopedOverride = strings.HasPrefix(low, "[mypy-") || low == "[[tool.mypy.overrides]]"
			continue
		}
		if !inScopedOverride && pyTypeWeakRe.MatchString(trimmed) {
			return true
		}
	}
	return false
}

func readPythonConfigs(inv repoInventory) (ruffFile, ruffText string, hasType bool, typeFile, typeText string) {
	if len(inv.configs["python_lint"]) > 0 {
		ruffFile = inv.configs["python_lint"][0]
		if b, ok := readSmallFile(filepath.Join(inv.root, filepath.FromSlash(ruffFile))); ok {
			ruffText = string(b)
		}
	}
	if len(inv.configs["python_type"]) > 0 {
		hasType = true
		typeFile = inv.configs["python_type"][0]
		if b, ok := readSmallFile(filepath.Join(inv.root, filepath.FromSlash(typeFile))); ok {
			typeText = string(b)
		}
	}
	for _, rel := range inv.configs["pyproject"] {
		b, ok := readSmallFile(filepath.Join(inv.root, filepath.FromSlash(rel)))
		if !ok {
			continue
		}
		for _, u := range pyprojectLintUnits(rel, b) {
			low := strings.ToLower(u.Text)
			if ruffFile == "" && strings.Contains(low, "[tool.ruff") {
				ruffFile, ruffText = rel, u.Text
			}
			if !hasType && (strings.Contains(low, "[tool.mypy") || strings.Contains(low, "[tool.pyright")) {
				hasType, typeFile, typeText = true, rel, u.Text
			}
		}
	}
	return ruffFile, ruffText, hasType, typeFile, typeText
}

func pyRuffNeedsHardening(text string) bool {
	if m := pyRuffIgnoreRe.FindStringSubmatch(text); m != nil && pyRuffWeakIgnRe.MatchString(m[1]) {
		return true
	}
	m := pyRuffSelectRe.FindStringSubmatch(text)
	if m == nil {
		return true
	}
	sel := m[1]
	hasB := strings.Contains(sel, `"B"`) || strings.Contains(sel, `'B'`) || strings.Contains(sel, `"ALL"`)
	hasMod := strings.Contains(sel, `"FURB"`) || strings.Contains(sel, `"UP"`) || strings.Contains(sel, `"SIM"`) || strings.Contains(sel, `"ALL"`)
	return !hasB || !hasMod
}

func auditShell(inv repoInventory) []DoctorFinding {
	if len(inv.configs["shell"]) > 0 {
		return nil
	}
	for _, wf := range inv.workflows {
		if b, ok := readSmallFile(filepath.Join(inv.root, filepath.FromSlash(wf))); ok && ciLinterCmdRe["shell"].Match(b) {
			return nil
		}
	}
	return []DoctorFinding{{
		Stack: "shell", ID: "shell-missing-shellcheck", Severity: "warning",
		Message: fmt.Sprintf("%d shell script(s) found with no .shellcheckrc or CI shellcheck step", inv.shellCount),
		FixHint: "add shellcheck to CI (on changed scripts) or add .shellcheckrc",
	}}
}

func auditCI(inv repoInventory, stacks []string) []DoctorFinding {
	if len(inv.workflows) == 0 {
		return []DoctorFinding{{
			Stack: "ci", ID: "ci-missing-workflow", Severity: "warning",
			Message: "no GitHub Actions workflow found in .github/workflows/",
			FixHint: "add .github/workflows/lint.yml running diff-gated linters on pull requests",
		}}
	}
	var allWF strings.Builder
	var out []DoctorFinding
	neuteredSeen := false
	for _, wf := range inv.workflows {
		b, ok := readSmallFile(filepath.Join(inv.root, filepath.FromSlash(wf)))
		if !ok {
			continue
		}
		text := string(b)
		allWF.WriteString(text)
		allWF.WriteByte('\n')
		if !neuteredSeen && (ciNeuteredStepRe.MatchString(text) || hasContinueOnErrorLinter(text)) {
			neuteredSeen = true
			out = append(out, DoctorFinding{
				Stack: "ci", ID: "ci-linter-neutered", Severity: "error", File: wf,
				Message: "CI workflow masks linter failures (continue-on-error: true, || true, or --max-warnings > 0)",
				FixHint: "fail CI on linter errors and use diff-gating (--new-from-rev / changed files) for legacy debt",
			})
		}
	}
	combined := allWF.String()
	var unwired []string
	for _, s := range stacks {
		if s == "shell" {
			continue // covered by shell-missing-shellcheck
		}
		if re, ok := ciLinterCmdRe[s]; ok && !re.MatchString(combined) {
			unwired = append(unwired, s)
		}
	}
	if len(unwired) > 0 {
		out = append(out, DoctorFinding{
			Stack: "ci", ID: "ci-linter-not-wired", Severity: "warning", File: inv.workflows[0],
			Message: fmt.Sprintf("CI workflows do not invoke a deterministic linter for: %s", strings.Join(unwired, ", ")),
			FixHint: "wire diff-gated linter checks for every stack in .github/workflows/",
		})
	}
	return out
}

// hasContinueOnErrorLinter checks per step and per job header so unrelated steps cannot bleed over.
func hasContinueOnErrorLinter(text string) bool {
	if !ciContinueErrRe.MatchString(text) {
		return false
	}
	for _, step := range splitWorkflowSteps(text) {
		if ciContinueErrRe.MatchString(step) && matchesAnyCILinter(step) {
			return true
		}
	}
	return hasJobContinueOnErrorLinter(text)
}

func matchesAnyCILinter(s string) bool {
	for _, re := range ciLinterCmdRe {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func hasJobContinueOnErrorLinter(text string) bool {
	for _, job := range splitWorkflowJobs(text) {
		if jobHasHeaderContinueOnError(job) && matchesAnyCILinter(job) {
			return true
		}
	}
	return false
}

func splitWorkflowJobs(text string) []string {
	var jobs, cur []string
	inJobs, jobIndent := false, -1
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if !inJobs {
			inJobs = indent == 0 && trimmed == "jobs:"
			continue
		}
		if indent == 0 {
			break
		}
		if isJobHeaderLine(trimmed, indent, jobIndent) {
			if len(cur) > 0 {
				jobs = append(jobs, strings.Join(cur, "\n"))
			}
			cur, jobIndent = []string{line}, indent
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		jobs = append(jobs, strings.Join(cur, "\n"))
	}
	return jobs
}

func isJobHeaderLine(trimmed string, indent, jobIndent int) bool {
	return (jobIndent < 0 || indent == jobIndent) && strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "-")
}

func jobHasHeaderContinueOnError(job string) bool {
	stepsIndent := -1
	for line := range strings.SplitSeq(job, "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if trimmed == "steps:" && stepsIndent < 0 {
			stepsIndent = indent
			continue
		}
		if (stepsIndent < 0 || indent <= stepsIndent) && ciContinueErrRe.MatchString(line) {
			return true
		}
	}
	return false
}

func splitWorkflowSteps(text string) []string {
	var steps []string
	var cur []string
	stepIndent := -1
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if strings.HasPrefix(trimmed, "- ") && (stepIndent < 0 || indent <= stepIndent) {
			if len(cur) > 0 {
				steps = append(steps, strings.Join(cur, "\n"))
			}
			cur = []string{line}
			stepIndent = indent
			continue
		}
		if stepIndent >= 0 && trimmed != "" && indent < stepIndent {
			steps = append(steps, strings.Join(cur, "\n"))
			cur, stepIndent = nil, -1
			continue
		}
		if len(cur) > 0 {
			cur = append(cur, line)
		}
	}
	if len(cur) > 0 {
		steps = append(steps, strings.Join(cur, "\n"))
	}
	return steps
}

// stripJSONComments removes // line comments and /* */ block comments for loose JSONC checks.
func stripJSONComments(src []byte) []byte {
	var out []byte
	for i := 0; i < len(src); i++ {
		if src[i] == '"' {
			end := scanJSONString(src, i)
			out = append(out, src[i:end]...)
			i = end - 1
			continue
		}
		if next, ok := skipJSONComment(src, i); ok {
			if src[i+1] == '/' && next < len(src) {
				out = append(out, '\n')
			}
			i = next
			continue
		}
		out = append(out, src[i])
	}
	return out
}

func scanJSONString(src []byte, start int) int {
	for i := start + 1; i < len(src); i++ {
		if src[i] == '\\' {
			i++
			continue
		}
		if src[i] == '"' {
			return i + 1
		}
	}
	return len(src)
}

func skipJSONComment(src []byte, i int) (int, bool) {
	if i+1 >= len(src) || src[i] != '/' {
		return i, false
	}
	if src[i+1] == '/' {
		j := i + 2
		for j < len(src) && src[j] != '\n' {
			j++
		}
		return j, true
	}
	if src[i+1] == '*' {
		j := i + 2
		for j+1 < len(src) && !(src[j] == '*' && src[j+1] == '/') {
			j++
		}
		return j + 1, true
	}
	return i, false
}

func writeDoctorReport(w io.Writer, rep DoctorReport, format string) {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}
	if len(rep.Stacks) == 0 {
		fmt.Fprintln(w, "lintdec doctor: no supported stacks detected (TS/JS, Go, Python, Shell)")
		return
	}
	fmt.Fprintf(w, "Detected stacks: %s\n", strings.Join(rep.Stacks, ", "))
	if len(rep.Suppressions) > 0 {
		var parts []string
		for _, k := range []string{"ts", "go", "python", "shell"} {
			if n := rep.Suppressions[k]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", k, n))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(w, "Inline suppressions: %s\n", strings.Join(parts, ", "))
		}
	}
	if rep.Fix != nil {
		writeDoctorFixSummary(w, *rep.Fix)
	}
	if len(rep.Findings) == 0 {
		fmt.Fprintln(w, "✓ All deterministic linter and CI checks look healthy.")
		return
	}
	fmt.Fprintf(w, "\nFindings (%d):\n", len(rep.Findings))
	for _, f := range rep.Findings {
		loc := f.Stack
		if f.File != "" {
			loc = f.Stack + " · " + f.File
		}
		fmt.Fprintf(w, "  [%s] %s (%s): %s\n        fix: %s\n", f.Severity, f.ID, loc, f.Message, f.FixHint)
	}
}

func writeDoctorFixSummary(w io.Writer, fix DoctorFixResult) {
	if fix.Summary != "" {
		fmt.Fprintf(w, "\nAuto-fix summary: %s\n", fix.Summary)
	}
	if len(fix.Planned) > 0 {
		fmt.Fprintf(w, "Dry-run planned files (%d): %s\n", len(fix.Planned), strings.Join(fix.Planned, ", "))
	}
	if len(fix.Written) > 0 {
		fmt.Fprintf(w, "Updated files (%d): %s\n", len(fix.Written), strings.Join(fix.Written, ", "))
	}
	for _, v := range fix.Validations {
		fmt.Fprintf(w, "  validate %s: %s\n", v.Path, v.Status)
	}
}
