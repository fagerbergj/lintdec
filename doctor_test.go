package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func findingIDs(rep DoctorReport) map[string]bool {
	out := map[string]bool{}
	for _, f := range rep.Findings {
		out[f.ID] = true
	}
	return out
}

func TestDoctorEmptyDir(t *testing.T) {
	dir := t.TempDir()
	rep, err := runDoctorAudit(dir)
	if err != nil {
		t.Fatalf("runDoctorAudit: %v", err)
	}
	if len(rep.Stacks) != 0 || len(rep.Findings) != 0 {
		t.Fatalf("expected empty report on empty dir, got %+v", rep)
	}
}

func TestDoctorMissingConfigsAndCI(t *testing.T) {
	dir := t.TempDir()
	writeTestFiles(t, dir, map[string]string{
		"go.mod":         "module example.com/demo\n\ngo 1.24\n",
		"main.go":        "package main\nfunc main() {}\n",
		"package.json":   `{"name":"demo","devDependencies":{"typescript":"^5.0.0"}}`,
		"src/app.ts":     "export const x = 1;\n",
		"pyproject.toml": "[project]\nname = \"demo\"\n",
		"app.py":         "def f():\n    return 1\n",
		"scripts/run.sh": "#!/usr/bin/env bash\necho hi\n",
	})
	rep, err := runDoctorAudit(dir)
	if err != nil {
		t.Fatalf("runDoctorAudit: %v", err)
	}
	ids := findingIDs(rep)
	for _, want := range []string{
		"go-missing-config",
		"ts-missing-linter",
		"py-missing-linter",
		"py-missing-typechecker",
		"shell-missing-shellcheck",
		"ci-missing-workflow",
	} {
		if !ids[want] {
			t.Errorf("missing expected finding %q; got %+v", want, rep.Findings)
		}
	}
}

func TestDoctorDetectsWeakConfigsAndNeuteredCI(t *testing.T) {
	dir := t.TempDir()
	writeTestFiles(t, dir, map[string]string{
		"go.mod":  "module example.com/demo\n",
		"main.go": "package main\n//nolint:errcheck\nfunc main() {}\n",
		".golangci.yaml": `version: "2"
linters:
  default: all
  disable:
    - errcheck
    - gosec
    - staticcheck
`,
		"package.json": `{"name":"demo","scripts":{"lint":"eslint ."}}`,
		"tsconfig.json": `{
  // comment in JSONC
  "compilerOptions": { "strict": false }
}`,
		"eslint.config.js": `export default [{
  linterOptions: { reportUnusedDisableDirectives: 'off' },
  rules: { '@typescript-eslint/no-explicit-any': 'warn' }
}];`,
		"src/app.ts": "export const x = 1;\n",
		"pyproject.toml": `[project]
name = "demo"

[tool.ruff.lint]
select = ["E", "F"]
ignore = ["F841"]

[tool.pyright]
typeCheckingMode = "basic"
`,
		"app.py": "def f():\n    return 1\n",
		".github/workflows/ci.yml": `name: CI
on: [push]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: golangci-lint run || true
      - run: npm run lint
        continue-on-error: true
`,
	})
	rep, err := runDoctorAudit(dir)
	if err != nil {
		t.Fatalf("runDoctorAudit: %v", err)
	}
	ids := findingIDs(rep)
	for _, want := range []string{
		"go-disabled-core-linters",
		"ts-tsconfig-not-strict",
		"ts-unused-disables-off",
		"py-ruff-weakened",
		"py-typechecker-weak",
		"ci-linter-neutered",
		"ci-linter-not-wired",
	} {
		if !ids[want] {
			t.Errorf("missing expected finding %q; got %+v", want, rep.Findings)
		}
	}
	if rep.Suppressions["go"] != 1 {
		t.Errorf("expected 1 go suppression, got %+v", rep.Suppressions)
	}
}

func TestDoctorCleanStrictRepo(t *testing.T) {
	dir := t.TempDir()
	writeTestFiles(t, dir, map[string]string{
		"go.mod":  "module example.com/clean\n",
		"main.go": "package main\nfunc main() {}\n",
		".golangci.yml": `version: "2"
linters:
  enable:
    - errcheck
    - govet
    - staticcheck
    - ineffassign
    - gosec
    - errorlint
    - modernize
    - nolintlint
`,
		"package.json":  `{"name":"clean","scripts":{"lint":"oxlint ."}}`,
		"tsconfig.json": `{"compilerOptions":{"strict":true}}`,
		".oxlintrc.json": `{
  "categories": { "correctness": "error", "suspicious": "error" },
  "rules": {
    "typescript/no-floating-promises": "error",
    "typescript/no-explicit-any": "error"
  }
}`,
		"src/app.ts": "export const x: number = 1;\n",
		"pyproject.toml": `[project]
name = "clean"

[tool.ruff.lint]
select = ["E", "F", "B", "SIM", "FURB", "UP", "RUF100"]

[tool.mypy]
strict = true
`,
		"app.py":         "def f() -> int:\n    return 1\n",
		"scripts/run.sh": "#!/usr/bin/env bash\nset -euo pipefail\necho ok\n",
		".github/workflows/ci.yml": `name: CI
on: [push, pull_request]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: golangci-lint run --new-from-rev=origin/main
      - run: npm run lint
      - run: ruff check .
      - run: mypy .
      - run: shellcheck scripts/*.sh
`,
	})
	rep, err := runDoctorAudit(dir)
	if err != nil {
		t.Fatalf("runDoctorAudit: %v", err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("expected clean strict repo to have 0 findings, got: %+v", rep.Findings)
	}
}

func TestDoctorIgnoresIncidentalScriptsAndSolutionTSConfig(t *testing.T) {
	dir := t.TempDir()
	writeTestFiles(t, dir, map[string]string{
		"go.mod":                   "module example.com/goonly\n",
		"main.go":                  "package main\nfunc main() {}\n",
		"scripts/one.py":           "print('helper')\n",
		".golangci.yml":            "version: \"2\"\nlinters:\n  default: all\n",
		"web/package.json":         `{"name":"ui"}`,
		"web/tsconfig.json":        `{"files":[],"references":[{"path":"./tsconfig.app.json"}]}`,
		"web/tsconfig.app.json":    `{"compilerOptions":{"strict":true}}`,
		"web/.oxlintrc.json":       `{"categories":{"correctness":"error"}}`,
		".github/workflows/ci.yml": "jobs:\n  lint:\n    steps:\n      - run: golangci-lint run\n      - run: npm run typecheck\n",
	})
	rep, err := runDoctorAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range rep.Stacks {
		if s == "python" {
			t.Fatalf("single incidental helper.py should not trigger python stack: %+v", rep.Stacks)
		}
	}
	if ids := findingIDs(rep); ids["ts-tsconfig-not-strict"] {
		t.Fatalf("solution tsconfig with strict tsconfig.app.json should not trigger ts-tsconfig-not-strict: %+v", rep.Findings)
	}
}

func TestDoctorWritablePathAllowlist(t *testing.T) {
	allowed := []string{
		".golangci.yml",
		".golangci.yaml",
		"eslint.config.mjs",
		".oxlintrc.json",
		"biome.json",
		"ruff.toml",
		"pyproject.toml",
		"tsconfig.json",
		"pyrightconfig.json",
		"mypy.ini",
		".shellcheckrc",
		".github/workflows/lint.yml",
		".github/workflows/ci.yaml",
	}
	for _, p := range allowed {
		if !isDoctorWritablePath(p) {
			t.Errorf("expected %q to be writable by doctor --fix", p)
		}
	}
	rejected := []string{
		"../.golangci.yml",
		"/etc/passwd",
		".git/config",
		"src/app.ts",
		"main.go",
		"package.json",
		".env",
		"scripts/deploy.sh",
		".github/workflows/../../evil.yml",
	}
	for _, p := range rejected {
		if isDoctorWritablePath(p) {
			t.Errorf("expected %q to be rejected by doctor --fix allowlist", p)
		}
	}
}

func TestResolveDoctorChatProvider(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("LINTDEC_PROVIDER", "")
	t.Setenv("LINTDEC_MODEL", "")
	t.Setenv("LINTDEC_BASE_URL", "")
	t.Setenv("LINTDEC_API_KEY", "")

	// Only systemone configured -> clear error.
	cfg := Config{
		Active: "clef",
		Providers: map[string]Provider{
			"clef": {BaseURL: "http://127.0.0.1:11436/upstream/clef", Protocol: "systemone", Models: []string{"clef"}},
		},
	}
	if err := cfg.save(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveDoctorChatProvider(doctorOpts{}, ProjectConfig{}); err == nil || !strings.Contains(err.Error(), "chat") {
		t.Fatalf("expected chat-model error when only systemone is configured, got %v", err)
	}

	// Active is systemone, plus one openai provider -> auto-selects openai provider.
	cfg.Providers["chat"] = Provider{BaseURL: "http://127.0.0.1:11435/v1", Protocol: "openai", Models: []string{"qwen3"}}
	if err := cfg.save(); err != nil {
		t.Fatal(err)
	}
	ep, note, err := resolveDoctorChatProvider(doctorOpts{}, ProjectConfig{})
	if err != nil {
		t.Fatalf("expected fallback to unique chat provider, got %v", err)
	}
	if ep.BaseURL != "http://127.0.0.1:11435/v1" || ep.Model != "qwen3" || !strings.Contains(note, "chat") {
		t.Fatalf("unexpected fallback endpoint: %+v note=%q", ep, note)
	}

	// Explicit --protocol systemone must still fail.
	if _, _, err := resolveDoctorChatProvider(doctorOpts{baseURL: "http://x", protocol: "systemone", model: "clef"}, ProjectConfig{}); err == nil {
		t.Fatal("expected error for explicit systemone protocol on doctor --fix")
	}
}

func TestDoctorFixWritesAndDryRun(t *testing.T) {
	dir := t.TempDir()
	writeTestFiles(t, dir, map[string]string{
		"go.mod":  "module example.com/fixme\n",
		"main.go": "package main\nfunc main() {}\n",
	})

	planJSON := `{
  "summary": "Added strict v2 .golangci.yml and diff-gated CI workflow",
  "files": [
    {
      "path": ".golangci.yml",
      "content": "version: \"2\"\nlinters:\n  enable:\n    - errcheck\n    - govet\n    - staticcheck\n    - ineffassign\n    - gosec\n    - errorlint\n    - modernize\n    - nolintlint\n"
    },
    {
      "path": ".github/workflows/lint.yml",
      "content": "name: Lint\non: [pull_request]\njobs:\n  lint:\n    runs-on: ubuntu-latest\n    steps:\n      - run: golangci-lint run --new-from-rev=origin/main\n"
    }
  ]
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": planJSON}},
			},
		})
	}))
	defer srv.Close()

	origValidator := validateDoctorFile
	validateDoctorFile = func(root, rel string) (string, error) { return "ok", nil }
	defer func() { validateDoctorFile = origValidator }()

	ep := endpoint{BaseURL: srv.URL, Protocol: "openai", Model: "test-model"}
	rep, err := runDoctorAudit(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Dry-run must not create any files.
	resDry, err := applyDoctorFix(context.Background(), dir, rep, ep, 0, true)
	if err != nil {
		t.Fatalf("dry-run fix failed: %v", err)
	}
	if len(resDry.Planned) != 2 {
		t.Fatalf("expected 2 planned files, got %+v", resDry)
	}
	if _, err := os.Stat(filepath.Join(dir, ".golangci.yml")); !os.IsNotExist(err) {
		t.Fatal("dry-run must not write .golangci.yml")
	}

	// Real write creates files directly and makes re-audit clean.
	res, err := applyDoctorFix(context.Background(), dir, rep, ep, 0, false)
	if err != nil {
		t.Fatalf("applyDoctorFix failed: %v", err)
	}
	if len(res.Written) != 2 {
		t.Fatalf("expected 2 written files, got %+v", res)
	}
	after, err := runDoctorAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Findings) != 0 {
		t.Fatalf("expected 0 findings after fix, got %+v", after.Findings)
	}
}

func TestDoctorFixRejectsDisallowedPathFromLLM(t *testing.T) {
	dir := t.TempDir()
	writeTestFiles(t, dir, map[string]string{
		"go.mod":  "module example.com/fixme\n",
		"main.go": "package main\nfunc main() {}\n",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"summary":"bad","files":[{"path":"main.go","content":"hacked"}]}`}},
			},
		})
	}))
	defer srv.Close()

	ep := endpoint{BaseURL: srv.URL, Protocol: "openai", Model: "test-model"}
	rep, _ := runDoctorAudit(dir)
	if _, err := applyDoctorFix(context.Background(), dir, rep, ep, 0, false); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected path allowlist error, got %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(got) != "package main\nfunc main() {}\n" {
		t.Fatalf("disallowed file was modified: %q", string(got))
	}
}
