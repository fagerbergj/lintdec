package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

// Unit is one lintable slice of a file. Comments carry surrounding code as
// Context because the judgment needs it; everything else is self-contained.
type Unit struct {
	Kind    string // TestFunction, Function, Interface, Comment, Paragraph, Bullet, File
	File    string
	Start   int
	End     int
	Text    string
	Context string
}

// rawBudgetLines: units at or below this go to the model verbatim; above it,
// local rules get windows and whole rules get a digest.
const rawBudgetLines = 200

func splitFile(path string, src []byte) []Unit {
	switch {
	case strings.HasSuffix(path, ".go"):
		return splitGo(path, src)
	case strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".markdown"):
		return splitMarkdown(path, src)
	}
	if key, ok := tsExt[extOf(path)]; ok {
		return splitTree(path, src, tsLangs[key])
	}
	return nil
}

func extOf(path string) string {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return ""
	}
	return strings.ToLower(path[i:])
}

func splitGo(path string, src []byte) []Unit {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: %s: %v (skipped)\n", path, err)
		return nil
	}
	lines := strings.Split(string(src), "\n")
	var units []Unit
	add := func(kind string, a, b token.Pos, ctx string) {
		s, e := fset.Position(a).Line, fset.Position(b).Line
		if e > len(lines) {
			e = len(lines)
		}
		units = append(units, Unit{Kind: kind, File: path, Start: s, End: e, Text: strings.Join(lines[s-1:e], "\n"), Context: ctx})
	}

	// File skeleton: one unit per Go file, declarations with line ranges.
	var sk []string
	sk = append(sk, fmt.Sprintf("file %s (lines 1-%d)", path, len(lines)))
	for _, d := range f.Decls {
		ln := fset.Position(d.Pos()).Line
		first := strings.TrimSpace(lines[ln-1])
		if len(first) > 120 {
			first = first[:120] + " …"
		}
		sk = append(sk, fmt.Sprintf("%d\t%s", ln, first))
	}
	units = append(units, Unit{Kind: "File", File: path, Start: 1, End: len(lines), Text: strings.Join(sk, "\n")})

	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			kind := "Function"
			if strings.HasPrefix(d.Name.Name, "Test") || strings.HasPrefix(d.Name.Name, "Fuzz") {
				kind = "TestFunction"
			}
			end := d.Type.End()
			if d.Body != nil {
				end = d.Body.End()
			}
			add(kind, d.Name.Pos(), end, "")
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if ok && ts.Type != nil {
					if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
						add("Interface", ts.Pos(), ts.Type.End(), "")
					}
				}
			}
		}
	}

	for _, g := range f.Comments {
		s, e := fset.Position(g.Pos()).Line, fset.Position(g.End()).Line
		var ctx []string
		for ln := s - 8; ln <= e+8; ln++ {
			if ln < 1 || ln > len(lines) || (ln >= s && ln <= e) {
				continue
			}
			if t := strings.TrimSpace(lines[ln-1]); t != "" {
				ctx = append(ctx, fmt.Sprintf("%d\t%s", ln, lines[ln-1]))
			}
		}
		add("Comment", g.Pos(), g.End(), strings.Join(ctx, "\n"))
	}
	return units
}

func splitMarkdown(path string, src []byte) []Unit {
	lines := strings.Split(string(src), "\n")
	var units []Unit
	// YAML front matter at the top of the file is metadata, not prose
	start := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
				start = i + 1
				break
			}
		}
	}
	fence := false
	i, n := start, len(lines)
	for i < n {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "```") {
			fence = !fence
			i++
			continue
		}
		if fence || t == "" || strings.HasPrefix(t, "#") {
			i++
			continue
		}
		start := i
		switch {
		case strings.HasPrefix(t, "|"):
			// table block: first-class unit; only table rules may apply
			for i < n && strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
				i++
			}
			units = append(units, Unit{Kind: "Table", File: path, Start: start + 1, End: i, Text: strings.Join(lines[start:i], "\n")})
			continue
		case isBullet(t):
			for i < n {
				t2 := strings.TrimSpace(lines[i])
				if t2 == "" || strings.HasPrefix(t2, "#") || strings.HasPrefix(t2, "```") ||
					(!isBullet(t2) && !isIndented(lines[i])) {
					break
				}
				i++
			}
		default:
			for i < n {
				t2 := strings.TrimSpace(lines[i])
				if t2 == "" || strings.HasPrefix(t2, "#") || isBullet(t2) || strings.HasPrefix(t2, "```") {
					break
				}
				i++
			}
		}
		units = append(units, Unit{Kind: mdKind(isBullet(strings.TrimSpace(lines[start]))), File: path, Start: start + 1, End: i, Text: strings.Join(lines[start:i], "\n")})
	}
	return units
}

func mdKind(bullet bool) string {
	if bullet {
		return "Bullet"
	}
	return "Paragraph"
}

func isBullet(t string) bool {
	for _, p := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	// numbered list: "1. " or "1) "
	if len(t) >= 2 && t[0] >= '0' && t[0] <= '9' {
		for j := 1; j < len(t); j++ {
			if t[j] == '.' || t[j] == ')' {
				return j+1 < len(t) && t[j+1] == ' '
			}
			if t[j] < '0' || t[j] > '9' {
				return false
			}
		}
	}
	return false
}

func isIndented(line string) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

// contentFor picks what a whole-scope rule sees for an oversized unit: a
// line-numbered digest of signature/branch/assert/return lines. Local rules
// get raw windows instead (see lint.go).
func digestFor(u Unit, lines []string) string {
	var out []string
	for ln := u.Start; ln <= u.End; ln++ {
		raw := lines[ln-1]
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		keep := ln <= u.Start+2
		if !keep {
			switch {
			case strings.HasPrefix(s, "if "), strings.HasPrefix(s, "switch "), strings.HasPrefix(s, "for "),
				strings.HasPrefix(s, "return "), strings.HasPrefix(s, "defer "), strings.HasPrefix(s, "go "),
				strings.Contains(s, "assert."), strings.Contains(s, "require."),
				strings.Contains(s, "t.Error"), strings.Contains(s, "t.Fatal"), strings.Contains(s, "t.Log"):
				keep = true
			}
		}
		if keep {
			out = append(out, fmt.Sprintf("%d\t%s", ln, strings.TrimRight(raw, " \t")))
		}
	}
	return strings.Join(out, "\n")
}
