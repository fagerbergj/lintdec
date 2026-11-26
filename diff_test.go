package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestDiffIncludesBranchIndexWorktreeAndUntracked(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "config", "user.email", "test@example.invalid")
	gitTest(t, dir, "config", "user.name", "Test")
	path := filepath.Join(dir, "sample.go")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package p\nfunc first() int {\n return 1\n}\nfunc second() int {\n return 2\n}\n")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "base")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	write("package p\nfunc first() int {\n return 10\n}\nfunc second() int {\n return 2\n}\n")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "branch")
	write("package p\nfunc first() int {\n return 11\n}\nfunc second() int {\n return 2\n}\n")
	gitTest(t, dir, "add", ".")
	write("package p\nfunc first() int {\n return 11\n}\nfunc second() int {\n return 20\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package p\nfunc third() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes, err := collectDiff(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if !changes[path].changed(3) || !changes[path].changed(6) || !changes[filepath.Join(dir, "new.go")].changed(2) {
		t.Fatalf("missing branch, working tree, or untracked changes: %#v", changes)
	}
}

func TestDiffDumpUnitsOnlyTouchedFunction(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "config", "user.email", "test@example.invalid")
	gitTest(t, dir, "config", "user.name", "Test")
	path := filepath.Join(dir, "sample.go")
	original := "package p\nfunc first() int {\n return 1\n}\n\nfunc second() int {\n return 2\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "base")
	if err := os.WriteFile(path, []byte(strings.Replace(original, "return 1", "return 10", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var code int
	output := captureStdout(t, func() { code = lintCmd([]string{"--dump-units", "--diff", "HEAD"}) })
	if code != 0 || !strings.Contains(output, "sample.go:2-4") || strings.Contains(output, "sample.go:6-8") {
		t.Fatalf("diff dump code=%d output=%q", code, output)
	}
}

func newDiffRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "config", "user.email", "test@example.invalid")
	gitTest(t, dir, "config", "user.name", "Test")
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "base")
	return dir
}

func TestDiffIncludesAllWindowsOfTouchedFunction(t *testing.T) {
	original := "package p\nfunc TestThing() {\n" + strings.Repeat("_ = 1\n", 220) + "}\n"
	dir := newDiffRepo(t, map[string]string{"sample.go": original})
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(strings.Replace(original, "_ = 1", "_ = 2", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var code int
	out := captureStdout(t, func() { code = lintCmd([]string{"--dump-units", "--diff", "HEAD", "--rules", "flakey-test"}) })
	if code != 0 || !strings.Contains(out, "2 batches") {
		t.Fatalf("code=%d output=%q", code, out)
	}
}

func TestDiffForcesTextDespiteGitAttributes(t *testing.T) {
	dir := newDiffRepo(t, map[string]string{".gitattributes": "*.go -diff\n", "sample.go": "package p\nfunc f() {}\n"})
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("package p\nfunc f() { println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes, err := collectDiff(dir, "HEAD")
	if err != nil || !changes[filepath.Join(dir, "sample.go")].changed(2) {
		t.Fatalf("changes=%v err=%v", changes, err)
	}
}

func TestDiffCleanJSONAndInvalidPath(t *testing.T) {
	dir := newDiffRepo(t, map[string]string{"sample.go": "package p\n"})
	t.Chdir(dir)
	var code int
	out := captureStdout(t, func() { code = lintCmd([]string{"--diff", "HEAD", "--out", "json"}) })
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("code=%d output=%q", code, out)
	}
	if code := lintCmd([]string{"--diff", "HEAD", "missing.go"}); code == 0 {
		t.Fatal("missing path reported clean")
	}
}

func TestDiffWithNoApplicableRulesNeedsNoProvider(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LINTDEC_PROVIDER", "")
	t.Setenv("LINTDEC_BASE_URL", "")
	dir := newDiffRepo(t, map[string]string{"README.md": "Original.\n"})
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("Changed.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var code int
	out := captureStdout(t, func() { code = lintCmd([]string{"--diff", "HEAD", "--out", "json", "--rules", "flakey-test"}) })
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("code=%d output=%q", code, out)
	}
}

func TestDiffSelectsFunctionWhenAttachedDocChanges(t *testing.T) {
	original := "/** Old documentation. */\nexport function demo() { return 1; }\n"
	dir := newDiffRepo(t, map[string]string{"sample.js": original})
	if err := os.WriteFile(filepath.Join(dir, "sample.js"), []byte(strings.Replace(original, "Old", "New", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var code int
	out := captureStdout(t, func() { code = lintCmd([]string{"--dump-units", "--diff", "HEAD"}) })
	if code != 0 || !strings.Contains(out, "sample.js:2-2") {
		t.Fatalf("code=%d output=%q", code, out)
	}
}

func TestDiffDoesNotSilentlyScanOnEmptyBase(t *testing.T) {
	if code := lintCmd([]string{"--dump-units", "--diff="}); code == 0 {
		t.Fatal("empty diff base must not fall back to a full scan")
	}
}

func TestDiffRespectsDirectoryExclusionsAndExplicitFiles(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := os.Mkdir("vendor", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"foo.go", "vendor/dep.go"} {
		if err := os.WriteFile(name, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	changes := map[string]diffRanges{filepath.Join(dir, "foo.go"): {}, filepath.Join(dir, "vendor/dep.go"): {}}
	files, err := diffFiles(changes, []string{"."}, defaultInclude(), nil)
	if err != nil || len(files) != 1 || files[0] != "foo.go" {
		t.Fatalf("directory exclusions: %v, %v", files, err)
	}
	files, err = diffFiles(changes, []string{".", "foo.go"}, defaultInclude(), []string{"*.go"})
	if err != nil || len(files) != 1 || files[0] != "foo.go" {
		t.Fatalf("explicit file override: %v, %v", files, err)
	}
}

func TestDiffSelectsOnlyChangedUnits(t *testing.T) {
	changes := diffRanges{lines: []lineRange{{start: 12, end: 13}}}
	if !changes.intersects(10, 20) || changes.intersects(1, 9) {
		t.Fatal("wrong changed-unit overlap")
	}
}

func TestDiffSkipsSymlinkTargets(t *testing.T) {
	dir := newDiffRepo(t, map[string]string{"sample.go": "package p\n"})
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "outside.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	changes, err := collectDiff(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := changes[link]; exists {
		t.Fatal("diff would read a symlink target outside the repository")
	}
}

func TestDeletionTouchesNearbyUnitButDoesNotMarkUnchangedLines(t *testing.T) {
	changes, err := parseDiffHunks([]byte("@@ -5,2 +4,0 @@\n-old\n-old2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !changes.intersects(1, 5) || !changes.intersects(1, 4) || changes.changed(4) {
		t.Fatalf("incorrect deletion handling: %#v", changes)
	}
}

func TestDiffSelectsGoDocAndMultilineSignature(t *testing.T) {
	original := "package p\n// First is old.\nfunc\n First() {}\n\n// I is old.\ntype I interface { Do() }\n"
	for _, tc := range []struct{ old, new, want string }{
		{"First is old", "First is new", "sample.go:3-4"},
		{"func\n First", "func \n First", "sample.go:3-4"},
		{"I is old", "I is new", "sample.go:7-7"},
	} {
		t.Run(tc.want+tc.old, func(t *testing.T) {
			dir := newDiffRepo(t, map[string]string{"sample.go": original})
			if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(strings.Replace(original, tc.old, tc.new, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)
			var code int
			out := captureStdout(t, func() { code = lintCmd([]string{"--dump-units", "--diff", "HEAD"}) })
			if code != 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("code=%d output=%q, want %s", code, out, tc.want)
			}
		})
	}
}

func TestDiffWorktreeAndLiteralPaths(t *testing.T) {
	base := newDiffRepo(t, map[string]string{"a[1].go": "package p\nfunc f() {}\n", "a1.go": "package p\nfunc g() {}\n", "removed.go": "package p\n"})
	worktree := filepath.Join(t.TempDir(), "checkout")
	gitTest(t, base, "worktree", "add", "-b", "feature", worktree, "HEAD")
	worktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "a[1].go"), []byte("package p\nfunc f() { println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", "a[1].go")
	gitTest(t, worktree, "rm", "removed.go")
	if err := os.WriteFile(filepath.Join(worktree, "new name\n.go"), []byte("package p\nfunc h() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes, err := collectDiff(worktree, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !changes[filepath.Join(worktree, "a[1].go")].changed(2) || !changes[filepath.Join(worktree, "new name\n.go")].changed(2) {
		t.Fatalf("missing worktree changes: %v", changes)
	}
	if _, ok := changes[filepath.Join(worktree, "removed.go")]; ok {
		t.Fatalf("deleted file included: %v", changes)
	}
	if _, ok := changes[filepath.Join(worktree, "a1.go")]; ok {
		t.Fatalf("literal path matched another file: %v", changes)
	}
	t.Chdir(worktree)
	files, err := diffFiles(changes, []string{"."}, defaultInclude(), []string{"new*"})
	if err != nil || !slices.Equal(files, []string{"a[1].go"}) {
		t.Fatalf("files=%q err=%v", files, err)
	}
	files, err = diffFiles(changes, []string{"*.go"}, defaultInclude(), nil)
	if err != nil || !slices.Equal(files, []string{"a[1].go", "new name\n.go"}) {
		t.Fatalf("worktree glob: files=%q err=%v", files, err)
	}
	files, err = diffFiles(changes, []string{"a?.go"}, defaultInclude(), nil)
	if err != nil || len(files) != 0 {
		t.Fatalf("unchanged glob: files=%q err=%v", files, err)
	}
}

func TestDiffRejectsInvalidRefWithoutScanning(t *testing.T) {
	dir := newDiffRepo(t, map[string]string{"sample.go": "package p\nfunc f() {}\n"})
	t.Chdir(dir)
	for _, ref := range []string{"not-a-ref", "--all"} {
		var code int
		out := captureStdout(t, func() { code = lintCmd([]string{"--dump-units", "--diff=" + ref}) })
		if code == 0 || strings.Contains(out, "sample.go") {
			t.Fatalf("ref=%q code=%d output=%q", ref, code, out)
		}
	}
}

func TestDiffJSONReportsWholeUnitAtNormalSeverity(t *testing.T) {
	original := "package p\nfunc TestFirst() {\n _ = 1\n _ = 2\n}\nfunc TestSecond() {}\n"
	dir := newDiffRepo(t, map[string]string{"sample_test.go": original})
	if err := os.WriteFile(filepath.Join(dir, "sample_test.go"), []byte(strings.Replace(original, "_ = 2", "_ = 3", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		content := `{"flakey-test":{"lint":true,"severity":"high","confidence":0.9,"line":3,"reason":"pre-existing issue"}}`
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": content}}}})
	}))
	defer srv.Close()
	args := []string{"--diff", "HEAD", "--out", "json", "--rules", "flakey-test", "--base-url", srv.URL, "--model", "test", "--no-cache", "--quiet"}
	var code int
	out := captureStdout(t, func() { code = lintCmd(args) })
	var findings []Finding
	if err := json.Unmarshal([]byte(out), &findings); err != nil || code != 1 || calls.Load() != 1 || len(findings) != 1 || findings[0].Severity != "error" || findings[0].Line != 2 {
		t.Fatalf("code=%d calls=%d output=%q findings=%+v err=%v", code, calls.Load(), out, findings, err)
	}
	out = captureStdout(t, func() { code = lintCmd(append(args, "--severity", "error")) })
	if code != 1 || !strings.Contains(out, `"severity": "error"`) {
		t.Fatalf("severity filter: code=%d output=%q", code, out)
	}
}

func TestDiffJSONEmptyAfterChecks(t *testing.T) {
	dir := newDiffRepo(t, map[string]string{"sample_test.go": "package p\nfunc TestFirst() {}\n"})
	if err := os.WriteFile(filepath.Join(dir, "sample_test.go"), []byte("package p\nfunc TestFirst() { println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := `{"incomplete-assertion":{"lint":true,"confidence":0.9,"line":2,"reason":"issue"}}`
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": content}}}})
	}))
	defer srv.Close()
	var code int
	out := captureStdout(t, func() {
		code = lintCmd([]string{"--diff", "HEAD", "--out", "json", "--rules", "incomplete-assertion", "--severity", "error", "--base-url", srv.URL, "--model", "test", "--no-cache", "--quiet"})
	})
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("code=%d output=%q", code, out)
	}
	out = captureStdout(t, func() {
		code = lintCmd([]string{"--out", "json", "--rules", "incomplete-assertion", "--severity", "error", "--base-url", srv.URL, "--model", "test", "--no-cache", "--quiet"})
	})
	if code != 0 || strings.TrimSpace(out) != "null" {
		t.Fatalf("non-diff output changed: code=%d output=%q", code, out)
	}
}

func TestDiffDeletionAndSubdirectoryPaths(t *testing.T) {
	original := "package p\nfunc first() {\n _ = 1\n _ = 2\n}\n\nfunc second() {}\n"
	dir := newDiffRepo(t, map[string]string{"sample.go": original, ".gitignore": "ignored.go\n"})
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(strings.Replace(original, " _ = 1\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "new.go"), []byte("package p\nfunc inner() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes, err := collectDiff(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !changes[filepath.Join(dir, "sample.go")].intersects(2, 4) || changes[filepath.Join(dir, "sample.go")].changed(3) {
		t.Fatalf("deletion ranges: %#v", changes)
	}
	if _, ok := changes[filepath.Join(dir, "ignored.go")]; ok {
		t.Fatal("ignored untracked file included")
	}
	t.Chdir(dir)
	var code int
	out := captureStdout(t, func() { code = lintCmd([]string{"--dump-units", "--diff", "HEAD", "sample.go"}) })
	if code != 0 || !strings.Contains(out, "sample.go:2-4") || strings.Contains(out, "sample.go:6-6") {
		t.Fatalf("deleted line unit selection: code=%d output=%q", code, out)
	}
	files, err := diffFiles(changes, []string{"ignored*.go"}, defaultInclude(), nil)
	if err != nil || len(files) != 0 {
		t.Fatalf("ignored glob: files=%q err=%v", files, err)
	}
	t.Chdir(filepath.Join(dir, "sub"))
	files, err = diffFiles(changes, []string{"*.go"}, defaultInclude(), nil)
	if err != nil || !slices.Equal(files, []string{"new.go"}) {
		t.Fatalf("subdirectory glob: files=%q err=%v", files, err)
	}
	files, err = diffFiles(changes, []string{"../sample.go"}, defaultInclude(), []string{"*.go"})
	if err != nil || !slices.Equal(files, []string{"../sample.go"}) {
		t.Fatalf("explicit parent file: files=%q err=%v", files, err)
	}
}

func TestDiffDoesNotFollowReplacedDirectorySymlink(t *testing.T) {
	dir := newDiffRepo(t, map[string]string{"placeholder": "x"})
	if err := os.Mkdir(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "pkg", "file.go")
	if err := os.WriteFile(inside, []byte("package p\nfunc f() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "pkg/file.go")
	gitTest(t, dir, "commit", "-m", "add package")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file.go"), []byte("package outside\nfunc f() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "pkg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "pkg")); err != nil {
		t.Fatal(err)
	}
	changes, err := collectDiff(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := changes[inside]; ok {
		t.Fatalf("would follow parent directory symlink: %#v", changes)
	}
}

func TestDiffUsesMergeBaseOnDivergedBranches(t *testing.T) {
	dir := newDiffRepo(t, map[string]string{"feature.go": "package p\nfunc f() {}\n", "other.go": "package p\nfunc g() {}\n"})
	gitTest(t, dir, "checkout", "-b", "other")
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package p\nfunc g() { println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "other.go")
	gitTest(t, dir, "commit", "-m", "other branch")
	gitTest(t, dir, "checkout", "-b", "feature", "main")
	if err := os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package p\nfunc f() { println(2) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "feature.go")
	gitTest(t, dir, "commit", "-m", "feature branch")
	changes, err := collectDiff(dir, "other")
	if err != nil || !changes[filepath.Join(dir, "feature.go")].changed(2) {
		t.Fatalf("changes=%v err=%v", changes, err)
	}
	if _, ok := changes[filepath.Join(dir, "other.go")]; ok {
		t.Fatalf("compared directly with other branch: %v", changes)
	}
}

func TestDiffSkipsOversizedUntrackedFile(t *testing.T) {
	dir := newDiffRepo(t, map[string]string{"sample.go": "package p\n"})
	if err := os.WriteFile(filepath.Join(dir, "huge.go"), []byte(strings.Repeat("x", (1<<20)+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var code int
	out := captureStdout(t, func() { code = lintCmd([]string{"--diff", "HEAD", "--dump-units"}) })
	if code != 0 || strings.Contains(out, "huge.go") || strings.Contains(out, "sample.go") {
		t.Fatalf("code=%d output=%q", code, out)
	}
}

func TestDiffIgnoresGitInterHunkContext(t *testing.T) {
	original := "package p\nfunc first() { println(1) }\nfunc middle() {}\nfunc last() { println(1) }\n"
	dir := newDiffRepo(t, map[string]string{"sample.go": original})
	gitTest(t, dir, "config", "diff.interHunkContext", "100")
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(strings.ReplaceAll(original, "println(1)", "println(2)")), 0o644); err != nil {
		t.Fatal(err)
	}
	changes, err := collectDiff(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	sample := changes[filepath.Join(dir, "sample.go")]
	if !sample.changed(2) || !sample.changed(4) {
		t.Fatalf("modified lines 2 and 4 must be marked changed: %#v", changes)
	}
	if sample.changed(3) {
		t.Fatalf("unchanged middle unit marked changed: %#v", changes)
	}
}

func TestCollectFilesSkipsGitIgnoredPaths(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	for name, body := range map[string]string{
		".gitignore": "build/\n*.gen.go\n", "a.go": "package a\n", "build/x.go": "package b\n", "z.gen.go": "package a\n",
		"tracked.gen.go": "package a\n", "sub/.gitignore": "local.md\n", "sub/local.md": "# local\n", "sub/keep.md": "# keep\n",
		"node_modules/m.md": "# dep\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", "-f", "tracked.gen.go")
	t.Chdir(dir)
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = stderr
	got, err := collectFiles([]string{"."}, defaultInclude(), nil)
	os.Stderr = orig
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go", "sub/keep.md", "tracked.gen.go"}; !slices.Equal(got, want) {
		t.Fatalf("collectFiles = %v, want %v", got, want)
	}
	logged, _ := os.ReadFile(stderr.Name())
	if string(logged) != "skip: build/ (git-ignored)\nskip: node_modules/ (always skipped)\n" {
		t.Fatalf("skipped directories not logged as expected: %q", logged)
	}
}
