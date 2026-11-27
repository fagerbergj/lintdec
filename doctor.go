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
	hasJS        bool
	hasGo        bool
	hasPython    bool
	hasShell     bool
	tsManifest   bool
	goManifest   bool
	pyManifest   bool
	tsCount      int
	goCount      int
	pyCount      int
	shellCount   int
	configs      map[string][]string
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
	tsRuleWeakRe     = regexp.MustCompile(`(?m)['"]?(?:@typescript-eslint/|typescript/)?(?:no-floating-promises|no-misused-promises|no-explicit-any|no-unused-vars|no-empty)['"]?\s*:\s*(?:['"](?:off|warn)['"]|[01]\b|\[\s*['"](?:off|warn)['"])`)
	goCoreLinters    = []string{"errcheck", "govet", "staticcheck", "ineffassign", "gosec", "errorlint", "modernize"}
	goDisableEntryRe = regexp.MustCompile(`(?m)^\s*-\s*([a-zA-Z0-9_-]+)\b`)
	pyRuffSelectRe   = regexp.MustCompile(`(?m)^\s*(?:extend-)?select\s*=\s*\[([^\]]*)\]`)
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
	rep, err := runDoctorAudit(o.root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if o.fix && len(rep.Findings) > 0 {
		code := runDoctorFixFlow(o, &rep)
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

func runDoctorFixFlow(o doctorOpts, rep *DoctorReport) int {
	pc, _ := loadProjectConfig(o.root)
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
	if !o.dryRun {
		after, err := runDoctorAudit(o.root)
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
With --fix, uses a configured chat (openai-protocol) LLM to generate or harden configs
and diff-gated CI workflows directly on disk (rely on git diff to review/revert).

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
	inv, err := scanDoctorRepo(root)
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
	if inv.hasShell {
		rep.Stacks = append(rep.Stacks, "shell")
		rep.Findings = append(rep.Findings, auditShell(inv)...)
	}
	if len(rep.Stacks) > 0 {
		rep.Findings = append(rep.Findings, auditCI(inv, rep.Stacks)...)
	}
	return rep, nil
}

func scanDoctorRepo(root string) (repoInventory, error) {
	inv := repoInventory{
		root:         root,
		configs:      map[string][]string{},
		suppressions: map[string]int{},
	}
	walkRoot(root, func(p string) {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return
		}
		rel = filepath.ToSlash(rel)
		classifyDoctorFile(&inv, p, rel)
	})
	for k := range inv.configs {
		slices.Sort(inv.configs[k])
	}
	slices.Sort(inv.workflows)
	return inv, nil
}

func classifyDoctorFile(inv *repoInventory, abs, rel string) {
	base := strings.ToLower(path.Base(rel))
	depth := strings.Count(rel, "/")
	if strings.HasPrefix(rel, ".github/workflows/") && (strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")) {
		inv.workflows = append(inv.workflows, rel)
		return
	}
	if depth <= 2 {
		recordDoctorConfig(inv, base, rel)
	}
	ext := extOf(rel)
	switch ext {
	case ".ts", ".tsx", ".mts", ".cts":
		inv.hasTS = true
		inv.tsCount++
		countFileSuppressions(inv, "ts", abs, doctorSuppTSRe)
	case ".js", ".jsx", ".mjs", ".cjs":
		inv.hasJS = true
		inv.tsCount++
		countFileSuppressions(inv, "ts", abs, doctorSuppTSRe)
	case ".go":
		inv.hasGo = true
		inv.goCount++
		countFileSuppressions(inv, "go", abs, doctorSuppGoRe)
	case ".py":
		inv.hasPython = true
		inv.pyCount++
		countFileSuppressions(inv, "python", abs, doctorSuppPyRe)
	case ".sh":
		inv.hasShell = true
		inv.shellCount++
		countFileSuppressions(inv, "shell", abs, doctorSuppShRe)
	case "":
		if src, ok := readSmallFile(abs); ok && isShellShebang(src) {
			inv.hasShell = true
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

func isPythonManifest(base string) bool {
	return base == "pyproject.toml" || base == "setup.py" || base == "setup.cfg" || strings.HasPrefix(base, "requirements")
}

func recordDoctorConfig(inv *repoInventory, base, rel string) {
	switch {
	case base == "package.json":
		inv.hasJS, inv.tsManifest = true, true
	case strings.HasPrefix(base, "tsconfig") && strings.HasSuffix(base, ".json"):
		inv.hasTS, inv.tsManifest = true, true
		inv.configs["tsconfig"] = append(inv.configs["tsconfig"], rel)
	case isTSLinterConfig(base):
		inv.configs["ts"] = append(inv.configs["ts"], rel)
	case base == "go.mod":
		inv.hasGo, inv.goManifest = true, true
	case strings.HasPrefix(base, ".golangci."):
		inv.configs["go"] = append(inv.configs["go"], rel)
	case base == "ruff.toml" || base == ".ruff.toml":
		inv.hasPython, inv.pyManifest = true, true
		inv.configs["python_lint"] = append(inv.configs["python_lint"], rel)
	case isPythonManifest(base):
		inv.hasPython, inv.pyManifest = true, true
		if base == "pyproject.toml" {
			inv.configs["pyproject"] = append(inv.configs["pyproject"], rel)
		}
	case base == "mypy.ini" || base == ".mypy.ini" || base == "pyrightconfig.json":
		inv.hasPython, inv.pyManifest = true, true
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
	if m := tsRuleWeakRe.FindString(text); m != "" {
		out = append(out, DoctorFinding{
			Stack: "ts", ID: "ts-core-rule-weakened", Severity: "warning", File: rel,
			Message: fmt.Sprintf("core correctness rule weakened (%s)", strings.TrimSpace(m)),
			FixHint: "enforce core TS/JS correctness rules at error severity (use diff-gated CI for legacy debt)",
		})
	}
	return out
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
	var out []DoctorFinding
	if len(inv.configs["go"]) == 0 {
		return []DoctorFinding{{
			Stack: "go", ID: "go-missing-config", Severity: "error",
			Message: "no .golangci.yml config found",
			FixHint: "add a v2 .golangci.yml enabling errcheck, govet, staticcheck, ineffassign, gosec, errorlint, modernize, and nolintlint",
		}}
	}
	rel := inv.configs["go"][0]
	src, ok := readSmallFile(filepath.Join(inv.root, filepath.FromSlash(rel)))
	if !ok {
		return out
	}
	text := string(src)
	disabled := parseGoDisabledLinters(text)
	var missingCore []string
	for _, name := range goCoreLinters {
		if disabled[name] {
			missingCore = append(missingCore, name)
		}
	}
	if len(missingCore) > 0 {
		out = append(out, DoctorFinding{
			Stack: "go", ID: "go-disabled-core-linters", Severity: "error", File: rel,
			Message: fmt.Sprintf("core Go linters disabled: %s", strings.Join(missingCore, ", ")),
			FixHint: "re-enable core linters and gate CI with golangci-lint run --new-from-rev=origin/main",
		})
	}
	hasDefaultAll := strings.Contains(text, "default: all") || strings.Contains(text, "enable-all: true")
	if disabled["nolintlint"] || (!hasDefaultAll && !strings.Contains(text, "nolintlint")) {
		out = append(out, DoctorFinding{
			Stack: "go", ID: "go-missing-nolintlint", Severity: "warning", File: rel,
			Message: "nolintlint is not enabled to require reasons on //nolint directives",
			FixHint: "enable nolintlint with require-explanation: true and require-specific: true",
		})
	}
	return out
}

func parseGoDisabledLinters(text string) map[string]bool {
	out := map[string]bool{}
	inDisable := false
	disableIndent := 0
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if trimmed == "disable:" {
			inDisable = true
			disableIndent = indent
			continue
		}
		if inDisable && indent <= disableIndent && !strings.HasPrefix(trimmed, "-") {
			inDisable = false
		}
		if inDisable {
			if m := goDisableEntryRe.FindStringSubmatch(line); m != nil {
				out[m[1]] = true
			}
		}
	}
	return out
}

func auditPython(inv repoInventory) []DoctorFinding {
	var out []DoctorFinding
	ruffFile, ruffText, hasTypeCfg, typeFile, typeText := readPythonConfigs(inv)
	if ruffFile == "" {
		out = append(out, DoctorFinding{
			Stack: "python", ID: "py-missing-linter", Severity: "error",
			Message: "no Ruff config found (ruff.toml or [tool.ruff] in pyproject.toml)",
			FixHint: `add [tool.ruff.lint] with select = ["E", "F", "B", "SIM", "FURB", "UP", "RUF100"]`,
		})
	} else {
		if pyRuffNeedsHardening(ruffText) {
			out = append(out, DoctorFinding{
				Stack: "python", ID: "py-ruff-weakened", Severity: "warning", File: ruffFile,
				Message: "Ruff config misses bugbear/modernization rules (B, FURB/SIM/UP, RUF100) or ignores core checks",
				FixHint: `select B, SIM, FURB, UP, RUF100 and avoid globally ignoring F841/F401/E722/ASYNC`,
			})
		}
	}
	if !hasTypeCfg {
		out = append(out, DoctorFinding{
			Stack: "python", ID: "py-missing-typechecker", Severity: "warning",
			Message: "no Python typechecker config found (mypy or pyright)",
			FixHint: "add [tool.mypy] with strict = true or [tool.pyright] with typeCheckingMode = \"standard\"",
		})
	} else if pyTypeWeakRe.MatchString(typeText) {
		out = append(out, DoctorFinding{
			Stack: "python", ID: "py-typechecker-weak", Severity: "warning", File: typeFile,
			Message: "Python typechecker is weakened (basic/off mode or strict = false)",
			FixHint: `enable strict = true (mypy) or typeCheckingMode = "standard"/"strict" (pyright)`,
		})
	}
	return out
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
	if pyRuffWeakIgnRe.MatchString(extractTomlKeyBlock(text, "ignore")) {
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

func extractTomlKeyBlock(text, key string) string {
	idx := strings.Index(text, key)
	if idx < 0 {
		return ""
	}
	rest := text[idx:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return ""
	}
	return rest[:end+1]
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

func hasContinueOnErrorLinter(text string) bool {
	if !ciContinueErrRe.MatchString(text) {
		return false
	}
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if !ciContinueErrRe.MatchString(ln) {
			continue
		}
		lo, hi := max(0, i-4), min(len(lines), i+3)
		window := strings.Join(lines[lo:hi], "\n")
		for _, re := range ciLinterCmdRe {
			if re.MatchString(window) {
				return true
			}
		}
	}
	return false
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
