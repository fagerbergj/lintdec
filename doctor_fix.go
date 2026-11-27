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
1. Only emit linter/typechecker config files (e.g. .golangci.yml, eslint.config.mjs, .oxlintrc.json, biome.json, ruff.toml, pyproject.toml, tsconfig.json, pyrightconfig.json, mypy.ini, .shellcheckrc) and GitHub Actions workflows (.github/workflows/*.yml). NEVER modify application source code or package.json.
2. Default to ratcheting / diff-gating in CI so strict rules enforce immediately on new/changed code without breaking legacy debt (e.g. golangci-lint run --new-from-rev=origin/main, ruff/eslint/oxlint/shellcheck on git diff changed files).
3. Preserve all existing non-linter settings when updating files like pyproject.toml, tsconfig.json, or existing CI workflows.
4. Enable correctness and anti-slop rules (errcheck, gosec, staticcheck, errorlint, modernize, nolintlint for Go; B, SIM, FURB, UP, RUF100 + strict mypy/pyright for Python; strict tsconfig, no-floating-promises, reportUnusedDisableDirectives, require-description for TS/JS; shellcheck for Shell). Never mask failures with || true or continue-on-error: true.
5. In GitHub Actions workflows, compute changed files with plain git diff (never third-party changed-files actions) and read language versions from project manifests (e.g. go-version-file: go.mod).`

var doctorExtraWritable = map[string]bool{
	"tsconfig.json": true, "pyrightconfig.json": true, "mypy.ini": true, ".mypy.ini": true,
}

// isDoctorWritablePath restricts --fix writes strictly to linter/typechecker configs and CI workflows.
func isDoctorWritablePath(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") {
		return false
	}
	clean := path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return false
	}
	if strings.HasPrefix(clean, ".git/") || clean == ".git" {
		return false
	}
	if strings.HasPrefix(clean, ".github/workflows/") {
		low := strings.ToLower(clean)
		return strings.Count(clean, "/") == 2 && (strings.HasSuffix(low, ".yml") || strings.HasSuffix(low, ".yaml"))
	}
	base := strings.ToLower(path.Base(clean))
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
	var existing []string
	for _, list := range rep.Configs {
		existing = append(existing, list...)
	}
	existing = append(existing, rep.Workflows...)
	slices.Sort(existing)
	existing = slices.Compact(existing)
	if len(existing) > 0 {
		sb.WriteString("\nEXISTING CONFIG AND WORKFLOW FILES:\n")
		budget := 24000
		for _, rel := range existing {
			b, ok := readSmallFile(filepath.Join(root, filepath.FromSlash(rel)))
			if !ok || len(b) > budget {
				continue
			}
			budget -= len(b)
			fmt.Fprintf(&sb, "\n--- %s ---\n%s\n", rel, string(b))
		}
	}
	return sb.String()
}

func applyDoctorFix(ctx context.Context, root string, rep DoctorReport, ep endpoint, temp float64, dryRun bool) (DoctorFixResult, error) {
	raw, err := callDoctorChat(ctx, ep, temp, buildDoctorFixPrompt(root, rep))
	if err != nil {
		return DoctorFixResult{}, err
	}
	plan, err := parseDoctorFixPlan(raw)
	if err != nil {
		return DoctorFixResult{}, err
	}
	for _, f := range plan.Files {
		if !isDoctorWritablePath(f.Path) {
			return DoctorFixResult{}, fmt.Errorf("doctor --fix refused path %q: not allowed (only linter/typechecker configs and CI workflows may be modified)", f.Path)
		}
		if strings.TrimSpace(f.Content) == "" {
			return DoctorFixResult{}, fmt.Errorf("doctor --fix refused empty content for %q", f.Path)
		}
	}
	res := DoctorFixResult{Summary: plan.Summary}
	if dryRun {
		for _, f := range plan.Files {
			res.Planned = append(res.Planned, f.Path)
		}
		return res, nil
	}
	for _, f := range plan.Files {
		clean := path.Clean(strings.ReplaceAll(f.Path, "\\", "/"))
		full := filepath.Join(root, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return res, err
		}
		if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
			return res, err
		}
		res.Written = append(res.Written, clean)
		status, verr := validateDoctorFile(root, clean)
		if verr != nil {
			status = "warn: " + verr.Error()
		}
		if status != "" {
			res.Validations = append(res.Validations, DoctorValidation{Path: clean, Status: status})
		}
	}
	return res, nil
}

func callDoctorChat(ctx context.Context, ep endpoint, temp float64, userPrompt string) (string, error) {
	body := map[string]any{
		"model": ep.Model, "temperature": temp, "max_tokens": 8192,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"messages": []map[string]string{
			{"role": "system", "content": doctorFixSystemPrompt},
			{"role": "user", "content": userPrompt},
		},
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(ep.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := authorizeRequest(req, ep.source, ep.APIKey); err != nil {
		return "", err
	}
	httpc := &http.Client{Timeout: 180 * time.Second}
	res, err := sendRequest(httpc, req, ep.source != nil || ep.APIKey != "")
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", errors.New(responseError(res, ep.source, ep.APIKey))
	}
	var usage *chatUsage
	return decodeCompletion(res.Body, &usage)
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

// validateDoctorFile checks syntax/schema of a written config file; injectable for tests.
var validateDoctorFile = defaultValidateDoctorFile

func defaultValidateDoctorFile(root, rel string) (string, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	base := strings.ToLower(path.Base(rel))
	switch {
	case strings.HasSuffix(base, ".json"):
		b, err := os.ReadFile(full)
		if err != nil {
			return "", err
		}
		if !json.Valid(stripJSONComments(b)) {
			return "", errors.New("invalid JSON syntax")
		}
		return "json valid", nil
	case strings.HasPrefix(base, ".golangci."):
		if _, err := exec.LookPath("golangci-lint"); err != nil {
			return "skipped (golangci-lint not installed)", nil
		}
		cmd := exec.Command("golangci-lint", "config", "verify", "--config", full)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("golangci-lint config verify: %s", strings.TrimSpace(string(out)))
		}
		return "golangci-lint config verify ok", nil
	case base == "eslint.config.js" || base == "eslint.config.mjs" || base == "eslint.config.cjs":
		if _, err := exec.LookPath("node"); err != nil {
			return "skipped (node not installed)", nil
		}
		cmd := exec.Command("node", "--check", full)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("node --check: %s", strings.TrimSpace(string(out)))
		}
		return "node --check ok", nil
	}
	return "", nil
}
