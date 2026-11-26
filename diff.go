package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type lineRange struct{ start, end int }

type diffRanges struct {
	lines   []lineRange
	touched []lineRange
}

func (d diffRanges) changed(line int) bool {
	for _, r := range d.lines {
		if r.start <= line && line <= r.end {
			return true
		}
	}
	return false
}

func (d diffRanges) intersects(start, end int) bool {
	for _, r := range d.lines {
		if r.start <= end && start <= r.end {
			return true
		}
	}
	for _, r := range d.touched {
		if r.start <= end && start <= r.end {
			return true
		}
	}
	return false
}

var hunkHeader = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`)

func parseDiffHunks(data []byte) (diffRanges, error) {
	var changes diffRanges
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		m := hunkHeader.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		start, err := strconv.Atoi(m[1])
		if err != nil {
			return changes, err
		}
		count := 1
		if m[2] != "" {
			count, err = strconv.Atoi(m[2])
			if err != nil {
				return changes, err
			}
		}
		if count == 0 {
			changes.touched = append(changes.touched, lineRange{max(start, 1), start + 1})
		} else {
			changes.lines = append(changes.lines, lineRange{start, start + count - 1})
		}
	}
	return changes, scanner.Err()
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(e.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

func collectDiff(dir, base string) (map[string]diffRanges, error) {
	if base == "" || strings.HasPrefix(base, "-") {
		return nil, fmt.Errorf("invalid diff base %q", base)
	}
	rootBytes, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	root := strings.TrimSuffix(string(rootBytes), "\n")
	mergeBase, err := gitOutput(root, "merge-base", base, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("cannot find merge base for %q: %w", base, err)
	}
	changes, err := trackedChanges(root, strings.TrimSpace(string(mergeBase)))
	if err != nil {
		return nil, err
	}
	untracked, err := gitOutput(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(string(untracked), "\x00") {
		if name != "" {
			path := filepath.Join(root, filepath.FromSlash(name))
			if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() {
				changes[path] = diffRanges{lines: []lineRange{{1, int(^uint(0) >> 1)}}}
			}
		}
	}
	return changes, nil
}

// trackedChanges: changed line ranges of regular tracked files since base, keyed by absolute path.
func trackedChanges(root, base string) (map[string]diffRanges, error) {
	tracked, err := gitOutput(root, "diff", "--no-ext-diff", "--no-color", "--no-renames", "--name-only", "--diff-filter=ACMT", "-z", base, "--")
	if err != nil {
		return nil, err
	}
	changes := make(map[string]diffRanges)
	for _, name := range strings.Split(string(tracked), "\x00") {
		if name == "" {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
			continue
		} else if st.Size() > 1<<20 {
			fmt.Fprintf(os.Stderr, "warn: %s: over 1MB, skipped\n", name)
			continue
		}
		patch, err := gitOutput(root, "--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--text", "--no-color", "--no-renames", "--unified=0", "--inter-hunk-context=0", base, "--", name)
		if err != nil {
			return nil, err
		}
		ranges, err := parseDiffHunks(patch)
		if err != nil {
			return nil, err
		}
		if len(ranges.lines)+len(ranges.touched) > 0 {
			changes[path] = ranges
		}
	}
	return changes, nil
}

func canonicalDiffPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func hasSkippedDir(path string) bool {
	for _, part := range strings.Split(filepath.Dir(path), string(filepath.Separator)) {
		if skipDirs[part] {
			return true
		}
	}
	return false
}

func diffFiles(changes map[string]diffRanges, paths, include, exclude []string) ([]string, error) {
	if err := checkDiffPaths(paths); err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}
	var files []string
	for path := range changes {
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		matched, explicit, err := diffPathSelected(path, rel, paths)
		if err != nil {
			return nil, err
		}
		if !matched || (!explicit && !fileSelected(path, rel, include, exclude)) {
			continue
		}
		if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() {
			continue
		} else if st.Size() > 1<<20 {
			fmt.Fprintf(os.Stderr, "warn: %s: over 1MB, skipped\n", rel)
			continue
		}
		files = append(files, rel)
	}
	slices.Sort(files)
	return files, nil
}

// checkDiffPaths: every positional path exists or is a glob that matches a repository file.
func checkDiffPaths(paths []string) error {
	var candidates []string
	for _, raw := range paths {
		if _, err := os.Stat(raw); err == nil {
			continue
		}
		if !strings.ContainsAny(raw, "*?[{") {
			return fmt.Errorf("no files matched: %s", raw)
		}
		if candidates == nil {
			var err error
			if candidates, err = repoFiles(); err != nil {
				return err
			}
		}
		if !slices.ContainsFunc(candidates, func(c string) bool { return globMatchesFile(raw, c) }) {
			return fmt.Errorf("no files matched: %s", raw)
		}
	}
	return nil
}

// repoFiles: tracked and untracked paths outside the always-skipped directories.
func repoFiles() ([]string, error) {
	args := []string{"ls-files", "--cached", "--others", "-z"}
	for dir := range skipDirs {
		args = append(args, "--exclude="+dir+"/")
	}
	out, err := gitOutput(".", args...)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(out), "\x00"), nil
}

func globMatchesFile(raw, candidate string) bool {
	if candidate == "" || hasSkippedDir(candidate) || !matchGlob(raw, filepath.ToSlash(candidate)) {
		return false
	}
	st, err := os.Stat(candidate)
	return err == nil && !st.IsDir()
}

// diffPathSelected: whether a changed file falls under the positional paths, and whether one names it exactly.
func diffPathSelected(path, rel string, paths []string) (matched, explicit bool, err error) {
	for _, raw := range paths {
		st, statErr := os.Stat(raw)
		if statErr != nil {
			matched = matched || (matchGlob(raw, rel) && !hasSkippedDir(rel))
			continue
		}
		abs, err := canonicalDiffPath(raw)
		if err != nil {
			return false, false, err
		}
		inside, relErr := filepath.Rel(abs, path)
		switch {
		case relErr != nil:
		case inside == "." && !st.IsDir():
			matched, explicit = true, true
		case st.IsDir() && inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator)):
			matched = matched || !hasSkippedDir(inside)
		}
	}
	return matched, explicit, nil
}
