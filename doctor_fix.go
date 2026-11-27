package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type DoctorFixFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type DoctorValidation struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type DoctorFixResult struct {
	Summary     string             `json:"summary"`
	Cost        string             `json:"cost,omitempty"`
	Planned     []string           `json:"planned,omitempty"`
	Written     []string           `json:"written,omitempty"`
	Validations []DoctorValidation `json:"validations,omitempty"`
}

type doctorFixPlan struct {
	Summary string          `json:"summary"`
	Files   []DoctorFixFile `json:"files"`
}

const doctorFixSystemPrompt = `You are an expert static-analysis engineer configuring deterministic linters and CI workflows.
Given a repository's detected stacks, doctor audit findings, and existing linter/CI config files, output ONLY a single JSON object (no markdown fences, no commentary):
{"summary": "<1-sentence summary>", "files": [{"path": "<relative path>", "content": "<complete file content>"}]}

Rules:
1. Emit ONLY standalone linter/typechecker configs (.golangci.yml, eslint.config.mjs, .oxlintrc.json, biome.json, ruff.toml, mypy.ini, pyrightconfig.json, .shellcheckrc) and GitHub Actions workflows (.github/workflows/*.yml). NEVER emit shared project manifests (pyproject.toml, tsconfig.json, package.json, lintdec.yaml) or application source code.
2. Put Python Ruff settings in ruff.toml and Mypy/Pyright settings in mypy.ini or pyrightconfig.json so existing pyproject.toml packaging metadata is never overwritten.
3. Default to ratcheting / diff-gating in CI so strict rules enforce immediately on new/changed code without breaking legacy debt (e.g. golangci-lint run --new-from-rev=origin/main, ruff/eslint/oxlint/shellcheck on git diff changed files).
4. Enable correctness and anti-slop rules (errcheck, gosec, staticcheck, errorlint, modernize, nolintlint for Go; B, SIM, FURB, UP, RUF100 + strict mypy/pyright for Python; no-floating-promises, reportUnusedDisableDirectives, require-description for TS/JS; shellcheck for Shell). Never mask failures with || true or continue-on-error: true.
5. In GitHub Actions workflows, compute changed files with plain git diff (never third-party changed-files actions), prefer official setup/linter actions (e.g. golangci/golangci-lint-action) over curl/wget | sh, never use pull_request_target or permissions: write-all, and read language versions from project manifests (e.g. go-version-file: go.mod).
6. Do not rewrite any existing file that was marked omitted or truncated in the prompt.`

// doctorSharedManifests carry build, dependency, or policy settings alongside lint config;
// --fix refuses them so standalone files (ruff.toml, mypy.ini, etc.) are written instead.
var doctorSharedManifests = map[string]bool{
	"pyproject.toml": true, "lintdec.yaml": true, "lintdec.yml": true,
}

var doctorExtraWritable = map[string]bool{
	"pyrightconfig.json": true, "mypy.ini": true, ".mypy.ini": true,
}

var (
	unsafeWorkflowTargetRe = regexp.MustCompile(`(?i)\bpull_request_target\b`)
	unsafeWorkflowPermRe   = regexp.MustCompile(`(?i)\bpermissions\s*:\s*write-all\b`)
	unsafeWorkflowPipeRe   = regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^\n|]*\|\s*(?:sudo\s+)?(?:ba|da|z)?sh\b`)
)

func cleanSafeRelPath(rel string) (string, bool) {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") {
		return "", false
	}
	clean := path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", false
	}
	if strings.HasPrefix(clean, ".git/") || clean == ".git" {
		return "", false
	}
	return clean, true
}

// isDoctorWritablePath restricts --fix writes strictly to standalone linter configs and CI workflows.
func isDoctorWritablePath(rel string) bool {
	clean, ok := cleanSafeRelPath(rel)
	if !ok {
		return false
	}
	if strings.HasPrefix(clean, ".github/workflows/") {
		low := strings.ToLower(clean)
		return strings.Count(clean, "/") == 2 && (strings.HasSuffix(low, ".yml") || strings.HasSuffix(low, ".yaml"))
	}
	base := strings.ToLower(path.Base(clean))
	if doctorSharedManifests[base] || (strings.HasPrefix(base, "tsconfig") && strings.HasSuffix(base, ".json")) {
		return false
	}
	return lintConfigNames[base] || doctorExtraWritable[base]
}

func resolveDoctorChatProvider(o doctorOpts, pc ProjectConfig) (endpoint, string, error) {
	lo := lintOpts{
		provider: o.provider, baseURL: o.baseURL, protocol: o.protocol,
		model: o.model, apiKey: o.apiKey,
	}
	if o.baseURL != "" || o.provider != "" || o.model != "" || o.protocol != "" {
		ep, err := resolveProvider(lo, pc)
		if err != nil {
			return ep, "", err
		}
		if ep.Protocol == "systemone" {
			return endpoint{}, "", errors.New("doctor --fix requires a chat (openai-protocol) model, not a systemone decision model")
		}
		return ep, "", nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return endpoint{}, "", err
	}
	if ep, err := resolveProvider(lo, pc); err == nil && ep.Protocol != "systemone" {
		return ep, "", nil
	}
	name, p, ok := findUniqueChatProvider(cfg)
	if !ok {
		return endpoint{}, "", errors.New("doctor --fix requires a configured chat (openai-protocol) model; run: lintdec provider add, or pass --provider / --base-url and --model")
	}
	lo.provider = name
	lo.model = p.defaultModel()
	ep, err := resolveProvider(lo, pc)
	if err != nil {
		return endpoint{}, "", err
	}
	return ep, fmt.Sprintf("doctor --fix: using chat provider %q (%s)", name, ep.Model), nil
}

func findUniqueChatProvider(cfg Config) (string, Provider, bool) {
	var names []string
	for name, p := range cfg.Providers {
		if p.Protocol != "systemone" && p.BaseURL != "" && p.defaultModel() != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) != 1 {
		return "", Provider{}, false
	}
	return names[0], cfg.Providers[names[0]], true
}

func buildDoctorFixPrompt(root string, rep DoctorReport) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "DETECTED STACKS: %s\n", strings.Join(rep.Stacks, ", "))
	sb.WriteString("FINDINGS TO FIX:\n")
	for _, f := range rep.Findings {
		fmt.Fprintf(&sb, "- [%s] %s (%s): %s | hint: %s\n", f.Severity, f.ID, f.File, f.Message, f.FixHint)
	}
	existing := collectExistingConfigPaths(rep)
	if len(existing) == 0 {
		return sb.String()
	}
	sb.WriteString("\nEXISTING CONFIG AND WORKFLOW FILES:\n")
	budget := 24000
	var omitted []string
	for _, rel := range existing {
		b, ok := readSmallFile(filepath.Join(root, filepath.FromSlash(rel)))
		if !ok {
			continue
		}
		if budget <= 0 {
			omitted = append(omitted, rel)
			continue
		}
		if len(b) > budget {
			fmt.Fprintf(&sb, "\n--- %s (truncated; do not rewrite whole file) ---\n%s\n... (truncated)\n", rel, string(b[:budget]))
			budget = 0
			continue
		}
		budget -= len(b)
		fmt.Fprintf(&sb, "\n--- %s ---\n%s\n", rel, string(b))
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&sb, "\nOMITTED DUE TO BUDGET (do not rewrite): %s\n", strings.Join(omitted, ", "))
	}
	return sb.String()
}

func collectExistingConfigPaths(rep DoctorReport) []string {
	var existing []string
	for _, list := range rep.Configs {
		existing = append(existing, list...)
	}
	existing = append(existing, rep.Workflows...)
	slices.Sort(existing)
	return slices.Compact(existing)
}

func applyDoctorFix(ctx context.Context, root string, rep DoctorReport, ep endpoint, temp float64, dryRun bool) (DoctorFixResult, error) {
	raw, costStr, err := callDoctorChat(ctx, ep, temp, buildDoctorFixPrompt(root, rep))
	if err != nil {
		return DoctorFixResult{}, err
	}
	plan, err := parseDoctorFixPlan(raw)
	if err != nil {
		return DoctorFixResult{}, err
	}
	kept, validations, err := preValidateDoctorPlan(root, plan.Files)
	if err != nil {
		return DoctorFixResult{}, err
	}
	res := DoctorFixResult{Summary: plan.Summary, Cost: costStr, Validations: validations}
	if dryRun {
		for _, f := range kept {
			res.Planned = append(res.Planned, path.Clean(strings.ReplaceAll(f.Path, "\\", "/")))
		}
		return res, nil
	}
	for _, f := range kept {
		clean := path.Clean(strings.ReplaceAll(f.Path, "\\", "/"))
		full := filepath.Join(root, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return res, err
		}
		if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
			return res, err
		}
		res.Written = append(res.Written, clean)
	}
	return res, nil
}

func preValidateDoctorPlan(root string, files []DoctorFixFile) ([]DoctorFixFile, []DoctorValidation, error) {
	var kept []DoctorFixFile
	var validations []DoctorValidation
	var skippedErr error
	for _, f := range files {
		if !isDoctorWritablePath(f.Path) {
			return nil, nil, fmt.Errorf("doctor --fix refused path %q: not allowed (only standalone linter/typechecker configs and CI workflows may be modified)", f.Path)
		}
		if strings.TrimSpace(f.Content) == "" {
			return nil, nil, fmt.Errorf("doctor --fix refused empty content for %q", f.Path)
		}
		clean := path.Clean(strings.ReplaceAll(f.Path, "\\", "/"))
		if err := verifyContainment(root, clean); err != nil {
			return nil, nil, fmt.Errorf("doctor --fix refused path %q: %w", clean, err)
		}
		status, err := validateDoctorContent(root, clean, []byte(f.Content))
		if err != nil {
			if strings.HasPrefix(clean, ".github/workflows/") {
				skippedErr = err
				validations = append(validations, DoctorValidation{Path: clean, Status: "skipped: " + err.Error()})
				continue
			}
			return nil, nil, fmt.Errorf("doctor --fix validation failed for %q (no files written): %w", clean, err)
		}
		if status != "" {
			validations = append(validations, DoctorValidation{Path: clean, Status: status})
		}
		kept = append(kept, f)
	}
	if len(kept) == 0 && skippedErr != nil {
		return nil, nil, fmt.Errorf("doctor --fix validation failed (no files written): %w", skippedErr)
	}
	return kept, validations, nil
}

// verifyContainment rejects symlink targets or symlinked parent dirs that resolve outside root.
func verifyContainment(root, clean string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return err
	}
	for probe := filepath.Join(absRoot, filepath.FromSlash(clean)); ; {
		if _, err := os.Lstat(probe); err == nil {
			resolved, err := filepath.EvalSymlinks(probe)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(realRoot, resolved)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return errors.New("path resolves outside repository root via symlink")
			}
			return nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return nil
		}
		probe = parent
	}
}

func callDoctorChat(ctx context.Context, ep endpoint, temp float64, userPrompt string) (string, string, error) {
	body := map[string]any{
		"model": ep.Model, "temperature": temp, "max_tokens": 8192,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"messages": []map[string]string{
			{"role": "system", "content": doctorFixSystemPrompt},
			{"role": "user", "content": userPrompt},
		},
	}
	payload, _ := json.Marshal(body)
	url := strings.TrimRight(ep.BaseURL, "/") + "/chat/completions"
	httpc := &http.Client{Timeout: 180 * time.Second}
	var costs costAccumulator
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
		if err != nil {
			return "", "", err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := authorizedDo(httpc, req, ep.source, ep.APIKey)
		if err != nil {
			return "", "", err
		}
		if (res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500) && attempt < 2 {
			res.Body.Close()
			select {
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
			continue
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return "", "", errors.New(responseError(res, ep.source, ep.APIKey))
		}
		var usage *chatUsage
		text, err := decodeCompletion(res.Body, &usage)
		costs.recordChat(usage, reportsProviderCost(ep.BaseURL))
		return text, formatCost(costs.snapshot()), err
	}
}

func parseDoctorFixPlan(raw string) (doctorFixPlan, error) {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '{' {
			continue
		}
		var plan doctorFixPlan
		if err := json.NewDecoder(strings.NewReader(raw[i:])).Decode(&plan); err == nil && len(plan.Files) > 0 {
			return plan, nil
		}
	}
	return doctorFixPlan{}, errors.New("doctor --fix: model response did not contain a valid JSON file plan")
}

// validateDoctorContent validates candidate config bytes before writing; injectable for tests.
var validateDoctorContent = defaultValidateDoctorContent

func defaultValidateDoctorContent(root, rel string, content []byte) (string, error) {
	base := strings.ToLower(path.Base(rel))
	if strings.HasPrefix(rel, ".github/workflows/") {
		return validateWorkflowContent(content)
	}
	switch {
	case strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".jsonc"):
		if !json.Valid(stripJSONComments(content)) {
			return "", errors.New("invalid JSON syntax")
		}
		return "json valid", nil
	case strings.HasPrefix(base, ".golangci."):
		return validateWithTempFile(root, base, content, "golangci-lint", func(tmp string) []string {
			return []string{"config", "verify", "--config", tmp}
		})
	case base == "eslint.config.js" || base == "eslint.config.mjs" || base == "eslint.config.cjs":
		return validateWithTempFile(root, base, content, "node", func(tmp string) []string {
			return []string{"--check", tmp}
		})
	}
	return "", nil
}

// validateWorkflowContent is a guardrail against common workflow footguns, not a security sandbox;
// review generated workflows with git diff before committing.
func validateWorkflowContent(content []byte) (string, error) {
	text := string(content)
	if unsafeWorkflowTargetRe.MatchString(text) {
		return "", errors.New("unsafe workflow trigger pull_request_target refused")
	}
	if unsafeWorkflowPermRe.MatchString(text) {
		return "", errors.New("unsafe workflow permissions: write-all refused")
	}
	if unsafeWorkflowPipeRe.MatchString(text) {
		return "", errors.New("unsafe remote script pipe (curl/wget | sh) refused")
	}
	if !strings.Contains(text, "jobs:") {
		return "", errors.New("workflow missing jobs: section")
	}
	return "workflow guard ok", nil
}

func validateWithTempFile(root, base string, content []byte, bin string, argsFn func(string) []string) (string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Sprintf("skipped (%s not installed)", bin), nil
	}
	tmpDir, err := os.MkdirTemp("", "lintdec-doctor-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	tmpFile := filepath.Join(tmpDir, base)
	if err := os.WriteFile(tmpFile, content, 0o600); err != nil {
		return "", err
	}
	cmd := exec.Command(bin, argsFn(tmpFile)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s failed: %s", bin, strings.TrimSpace(string(out)))
	}
	return bin + " ok", nil
}
