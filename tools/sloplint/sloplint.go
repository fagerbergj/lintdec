// sloplint for Go: comment runs, cyclomatic complexity and duplicated code on diff-touched lines.
// Policy mirrors quack's tools/sloplint.ts: runs of comment lines longer than maxCommentRun,
// function bodies with CC over maxCC, and clones of minTokens or more tokens (names erased,
// literals kept). Usage: go run ./tools/sloplint diff <git-ref> | repo
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const maxCommentRun = 2
const maxCC = 15
const minTokens = 80
const allow = "sloplint: cc-allow"

type span = [2]int

type finding struct {
	file    string
	line    int
	message string
}

func overlaps(s, e int, rs []span) bool {
	for _, r := range rs {
		if s <= r[1] && e >= r[0] {
			return true
		}
	}
	return false
}

var hunkRe = regexp.MustCompile(`^@@ -\S+ \+(\d+)(?:,(\d+))? @@`)

// Changed line ranges per file (new-file numbers) from `git diff -U0` output.
func changedRanges(diff string) map[string][]span {
	out := map[string][]span{}
	file := ""
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ ") {
			file = strings.TrimPrefix(line, "+++ ")
			file = strings.TrimPrefix(file, "b/")
			if file == "/dev/null" {
				file = ""
			}
			continue
		}
		m := hunkRe.FindStringSubmatch(line)
		if m == nil || file == "" {
			continue
		}
		start, _ := strconv.Atoi(m[1]) // hunk header line numbers are 1-based
		count := 1
		if m[2] != "" {
			count, _ = strconv.Atoi(m[2])
		}
		if count > 0 {
			out[file] = append(out[file], span{start, start + count - 1})
		}
	}
	return out
}

func changedSince(root, ref string) (map[string][]span, error) {
	cmd := exec.Command("git", "diff", "-U0", "--find-renames", "-l0", ref+"...HEAD")
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return changedRanges(out.String()), nil
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "repo" && (os.Args[1] != "diff" || len(os.Args) < 3)) {
		fmt.Fprintln(os.Stderr, "usage: sloplint <diff <git-ref> | repo>")
		os.Exit(2)
	}
	mode := os.Args[1]
	root, err := gitRoot()
	if err != nil {
		fatal(err)
	}
	tree, err := trackedGoFiles(root)
	if err != nil {
		fatal(err)
	}
	touched := map[string][]span{}
	if mode == "repo" {
		for f := range tree {
			touched[f] = nil // nil = whole file
		}
	} else {
		touched, err = changedSince(root, os.Args[2])
		if err != nil {
			fatal(err)
		}
	}
	var findings []finding
	for rel, rs := range touched {
		src, ok := tree[rel]
		if !ok {
			continue
		}
		findings = append(findings, lintSource(rel, src, rs)...)
	}
	findings = append(findings, clones(tree, touched)...)
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})
	for _, f := range findings {
		fmt.Printf("%s:%d: %s\n", f.file, f.line, f.message)
	}
	if len(findings) == 0 {
		fmt.Println("sloplint: ok")
	}
	if mode == "diff" {
		n := len(findings)
		if n > 1 {
			n = 1
		}
		os.Exit(n)
	}
}

func lintSource(rel string, src []byte, rs []span) []finding {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
	if err != nil {
		return nil
	}
	var out []finding
	out = append(out, commentRunFindings(rel, fset, f, rs)...)
	out = append(out, complexFindings(rel, fset, f, src, rs)...)
	return out
}

// Consecutive comment lines: a comment group covers the lines it physically spans, and a run
// longer than the limit that touches changed lines is a finding.
func commentRunFindings(rel string, fset *token.FileSet, f *ast.File, rs []span) []finding {
	n := fset.File(f.Pos()).LineCount()
	isComment := make([]bool, n+1)
	for _, g := range f.Comments {
		s := fset.Position(g.Pos()).Line
		e := fset.Position(g.End()).Line
		for l := s; l <= e; l++ {
			isComment[l] = true
		}
	}
	var out []finding
	l := 1
	for l <= n {
		if !isComment[l] {
			l++
			continue
		}
		e := l
		for e+1 <= n && isComment[e+1] {
			e++
		}
		if e-l+1 > maxCommentRun && (rs == nil || overlaps(l, e, rs)) {
			out = append(out, finding{file: rel, line: l, message: fmt.Sprintf("%d-line comment run touches changed lines", e-l+1)})
		}
		l = e + 1
	}
	return out
}

// Cyclomatic complexity of one function body: 1 per decision point (if, for, range, case,
// comm, &&, ||); nested closures do not inflate their enclosing function.
func complexity(body *ast.BlockStmt) int {
	cc := 1
	ast.Walk(&ccVisitor{cc: &cc}, body)
	return cc
}

type ccVisitor struct{ cc *int }

func (v *ccVisitor) Visit(n ast.Node) ast.Visitor {
	switch s := n.(type) {
	case *ast.FuncLit:
		return nil
	case *ast.IfStmt:
		*v.cc++
	case *ast.ForStmt, *ast.RangeStmt:
		*v.cc++
	case *ast.CaseClause, *ast.CommClause:
		*v.cc++
	case *ast.BinaryExpr:
		if s.Op == token.LAND || s.Op == token.LOR {
			*v.cc++
		}
	}
	return v
}

// The opt-out must name a reason, the way quack's does.
func ccAllowed(src []byte, topLine int) bool {
	lines := strings.Split(string(src), "\n")
	lo := topLine - 3
	if lo < 1 {
		lo = 1
	}
	for i := lo; i < topLine && i <= len(lines); i++ {
		t := strings.TrimSpace(lines[i-1])
		if !strings.HasPrefix(t, "//") || !strings.Contains(t, allow) {
			continue
		}
		reason := strings.TrimSpace(strings.SplitN(t, allow, 2)[1])
		if reason != "" {
			return true
		}
	}
	return false
}

func complexFindings(rel string, fset *token.FileSet, f *ast.File, src []byte, rs []span) []finding {
	var out []finding
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		s := fset.Position(fn.Body.Lbrace).Line
		e := fset.Position(fn.End()).Line
		cc := complexity(fn.Body)
		if cc > maxCC && !ccAllowed(src, fset.Position(fn.Pos()).Line) && (rs == nil || overlaps(s, e, rs)) {
			out = append(out, finding{file: rel, line: s, message: fmt.Sprintf("%s CC %d > %d (changed)", fn.Name.Name, cc, maxCC)})
		}
		return true
	})
	return out
}

// Tokens: names erased to one id, literals kept verbatim (or every boilerplate would match),
// structure and operators by kind, everything else skipped.
type tok struct {
	id   uint64
	line int
}

func tokens(fset *token.FileSet, f *ast.File, vocab map[string]uint64) []tok {
	key := func(k string) uint64 {
		if v, ok := vocab[k]; ok {
			return v
		}
		v := uint64(len(vocab) + 1)
		vocab[k] = v
		return v
	}
	var out []tok
	appendTok := func(k string, pos token.Pos) {
		out = append(out, tok{id: key(k), line: fset.Position(pos).Line})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		switch x := n.(type) {
		case *ast.Ident:
			appendTok("$name", n.Pos())
			return false
		case *ast.BasicLit:
			appendTok("lit:"+x.Value, n.Pos())
			return false
		case *ast.BinaryExpr:
			appendTok("op:"+x.Op.String(), n.Pos())
		case *ast.FuncDecl:
			appendTok("func", n.Pos())
		case *ast.IfStmt:
			appendTok("if", n.Pos())
		case *ast.ForStmt:
			appendTok("for", n.Pos())
		case *ast.RangeStmt:
			appendTok("range", n.Pos())
		case *ast.ReturnStmt:
			appendTok("return", n.Pos())
		case *ast.AssignStmt:
			appendTok("assign", n.Pos())
		case *ast.DeclStmt:
			appendTok("decl", n.Pos())
		case *ast.BlockStmt:
			appendTok("block", n.Pos())
		}
		return true
	})
	return out
}

// A 128-bit double-FNV window key over minTokens tokens: collisions stay out of reach without
// holding each window's text.
func windowKey(toks []tok, at int) [2]uint64 {
	a := fnv.New64a()
	b := fnv.New64()
	var buf [8]byte
	for i := at; i < at+minTokens; i++ {
		for j := 0; j < 8; j++ {
			buf[j] = byte(toks[i].id >> (8 * j))
		}
		a.Write(buf[:])
		for j := 0; j < 8; j++ {
			buf[j] = byte((toks[i].id + 0x9e3779b9) >> (8 * j))
		}
		b.Write(buf[:])
	}
	return [2]uint64{a.Sum64(), b.Sum64()}
}

// Exact token-window matches across the tree; untouched files are indexed first so a
// touched file copying an untouched file is caught regardless of path order.
func clones(tree map[string][]byte, touched map[string][]span) []finding {
	var untouched, changed []string
	for f := range tree {
		if _, ok := touched[f]; ok {
			changed = append(changed, f)
		} else {
			untouched = append(untouched, f)
		}
	}
	sort.Strings(untouched)
	sort.Strings(changed)
	files := append(untouched, changed...)
	vocab := map[string]uint64{}
	parsed := make([][]tok, len(files))
	for i, rel := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, tree[rel], 0)
		if err != nil {
			continue
		}
		parsed[i] = tokens(fset, f, vocab)
	}
	type loc struct {
		fileIdx int
		line    int
	}
	first := map[[2]uint64]loc{}
	var out []finding
	for i, rel := range files {
		toks := parsed[i]
		rs, isTouched := touched[rel]
		inRun := false
		for at := 0; at+minTokens <= len(toks); at++ {
			k := windowKey(toks, at)
			prior, seen := first[k]
			if !seen {
				first[k] = loc{fileIdx: i, line: toks[at].line}
				inRun = false
				continue
			}
			s, e := toks[at].line, toks[at+minTokens-1].line
			if !inRun && isTouched && (rs == nil || overlaps(s, e, rs)) {
				out = append(out, finding{
					file: rel, line: s,
					message: fmt.Sprintf("lines %d-%d duplicate %s:%d (%d+ tokens)", s, e, files[prior.fileIdx], prior.line, minTokens),
				})
				inRun = true
			}
		}
	}
	return out
}

func gitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Non-test .go files tracked by git; a file the diff names but the working tree no longer has
// (moved or deleted) has nothing left to lint.
func trackedGoFiles(root string) (map[string][]byte, error) {
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		return nil, err
	}
	tree := map[string][]byte{}
	for _, rel := range strings.Split(string(out), "\n") {
		if rel == "" || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		tree[rel] = data
	}
	return tree, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "sloplint:", err)
	os.Exit(2)
}
