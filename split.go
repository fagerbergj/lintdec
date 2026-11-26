package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Unit is one lintable slice of a file. Context is reference-only text the judgment
// needs: surrounding code for comments, the doc comment for declarations.
type Unit struct {
	Kind         string // TestFunction, Function, Interface, Comment, Paragraph, Bullet, File
	File         string
	Start        int
	End          int
	Text         string
	Context      string
	ContextStart int
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

	units = append(units, goSkeleton(fset, f, path, lines))

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
			add(kind, d.Pos(), end, "")
			if d.Doc != nil {
				units[len(units)-1].ContextStart = fset.Position(d.Doc.Pos()).Line
				units[len(units)-1].Context = strings.TrimSpace(d.Doc.Text())
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if ok && ts.Type != nil {
					if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
						add("Interface", ts.Pos(), ts.Type.End(), "")
						if doc := specDoc(d, ts); doc != nil {
							units[len(units)-1].ContextStart = fset.Position(doc.Pos()).Line
							units[len(units)-1].Context = strings.TrimSpace(doc.Text())
						}
					}
				}
			}
		}
	}

	for _, g := range f.Comments {
		add("Comment", g.Pos(), g.End(), goCommentContext(fset, f, lines, g))
	}
	return units
}

// goSkeleton: one unit per Go file listing declarations with line numbers.
func goSkeleton(fset *token.FileSet, f *ast.File, path string, lines []string) Unit {
	sk := []string{fmt.Sprintf("file %s (lines 1-%d)", path, len(lines))}
	for _, d := range f.Decls {
		ln := fset.Position(d.Pos()).Line
		first := strings.TrimSpace(lines[ln-1])
		if len(first) > 120 {
			first = first[:120] + " …"
		}
		sk = append(sk, fmt.Sprintf("%d\t%s", ln, first))
	}
	return Unit{Kind: "File", File: path, Start: 1, End: len(lines), Text: strings.Join(sk, "\n")}
}

// specDoc: a lone spec in a parenthesis-free declaration inherits the declaration's doc.
func specDoc(d *ast.GenDecl, ts *ast.TypeSpec) *ast.CommentGroup {
	if ts.Doc == nil && len(d.Specs) == 1 {
		return d.Doc
	}
	return ts.Doc
}

// goCommentContext: a comment in or above a declaration also sees all of it, so
// claims about code past the 8-line window can be checked.
func goCommentContext(fset *token.FileSet, f *ast.File, lines []string, g *ast.CommentGroup) string {
	s, e := fset.Position(g.Pos()).Line, fset.Position(g.End()).Line
	for _, d := range f.Decls {
		start := d.Pos()
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Doc != nil {
			start = fd.Doc.Pos()
		} else if gd, ok := d.(*ast.GenDecl); ok && gd.Doc != nil {
			start = gd.Doc.Pos()
		}
		ds, de := fset.Position(start).Line, min(fset.Position(d.End()).Line, len(lines))
		if ds <= s && e <= de && de-ds <= rawBudgetLines {
			return numberedContext(lines, min(ds, s-8), max(de, e+8), s, e)
		}
	}
	return commentContext(lines, s, e)
}

func splitMarkdown(path string, src []byte) []Unit {
	lines := strings.Split(string(src), "\n")
	var units []Unit
	fence := false
	i, n := frontMatterEnd(lines), len(lines)
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
		start, kind := i, mdKind(t)
		switch {
		case strings.HasPrefix(t, "|"): // table block: first-class unit; only table rules may apply
			i, kind = blockEnd(lines, i, func(l string) bool { return !strings.HasPrefix(strings.TrimSpace(l), "|") }), "Table"
		case isBullet(t):
			i = blockEnd(lines, i, func(l string) bool { return blockBreak(l) || (!isBullet(strings.TrimSpace(l)) && !isIndented(l)) })
		default:
			i = blockEnd(lines, i, func(l string) bool { return blockBreak(l) || isBullet(strings.TrimSpace(l)) })
		}
		units = append(units, Unit{Kind: kind, File: path, Start: start + 1, End: i, Text: strings.Join(lines[start:i], "\n")})
	}
	return addMarkdownStructure(path, lines, units)
}

// frontMatterEnd: the first line after YAML front matter, which is metadata, not prose.
func frontMatterEnd(lines []string) int {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
			return i + 1
		}
	}
	return 0
}

// blockEnd: the first line from i that stops the block.
func blockEnd(lines []string, i int, stop func(string) bool) int {
	for i < len(lines) && !stop(lines[i]) {
		i++
	}
	return i
}

// blockBreak: a blank line, heading, or fence ends any prose block.
func blockBreak(l string) bool {
	t := strings.TrimSpace(l)
	return t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "```")
}

// mdWholeLines caps Section and Document units, which are always sent whole.
// shortcut: longer ones are skipped, upgrade to an outline digest if big docs matter.
const mdWholeLines = 400

type mdHeading struct{ level, line int }

// addMarkdownStructure gives each block its heading path, previous block, and next line,
// and adds a Section unit per H2+ heading and one Document unit per file.
func addMarkdownStructure(path string, lines []string, units []Unit) []Unit {
	heads, paths := headingPaths(lines)
	units = slices.DeleteFunc(units, notAuthorProse)
	header := docHeader(lines)
	for k := range units {
		u := &units[k]
		ctx := header + "\nHEADING: " + paths[u.Start]
		if k == 0 {
			ctx += "\nPOSITION: first block of the document"
		}
		if k > 0 && paths[units[k-1].Start] == paths[u.Start] {
			ctx += "\nPREVIOUS BLOCK:\n" + units[k-1].Text
		}
		if next := nextLine(lines, u.End); next != "" {
			ctx += "\nNEXT LINE: " + next
		}
		u.Context = ctx
	}
	prose := slices.DeleteFunc(slices.Clone(units), func(u Unit) bool { return u.Kind != "Paragraph" && u.Kind != "Bullet" && u.Kind != "Steps" })
	var outline []string
	for _, h := range heads {
		outline = append(outline, strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[h.line-1]), "#")))
	}
	for _, u := range prose {
		units = append(units, candidateUnits(u, header, paths[u.Start], strings.Join(outline, " | "))...)
		if m, ok := mixedList(u); ok && u.Kind != "Paragraph" {
			units = append(units, m)
		}
	}
	text := proseLines(lines)
	units = append(units, sectionUnits(path, text, heads, header)...)
	if len(units) > 0 && len(lines) < mdWholeLines {
		units = append(units, Unit{Kind: "Document", File: path, Start: 1, End: len(lines), Text: strings.Join(text, "\n"), Context: header})
	}
	return units
}

// proseLines blanks front matter and fenced code bodies while leaving a placeholder on each
// opening fence so Section/Document rules see that code exists without linting code as prose.
func proseLines(lines []string) []string {
	out := slices.Clone(lines)
	fence, meta := false, frontMatterEnd(lines)
	for i := range out {
		if i < meta {
			out[i] = ""
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(out[i]), "```") {
			if fence {
				out[i] = ""
			} else {
				out[i] = fencePlaceholder(out[i])
			}
			fence = !fence
		} else if fence {
			out[i] = ""
		}
	}
	return out
}

func fencePlaceholder(line string) string {
	if fs := strings.Fields(strings.TrimLeft(strings.TrimSpace(line), "`")); len(fs) > 0 {
		return "[" + fs[0] + " code block]"
	}
	return "[code block]"
}

func isCodePlaceholder(t string) bool {
	if t == "[code block]" {
		return true
	}
	inner, ok := strings.CutPrefix(t, "[")
	if !ok {
		return false
	}
	lang, ok := strings.CutSuffix(inner, " code block]")
	return ok && lang != "" && !strings.ContainsAny(lang, " \t")
}

// headingPaths: the headings outside code fences, and the "A > B" heading path in force at each line.
func headingPaths(lines []string) ([]mdHeading, []string) {
	var heads []mdHeading
	paths := make([]string, len(lines)+1)
	var stack []string
	fence := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			fence = !fence
		}
		if lvl := len(t) - len(strings.TrimLeft(t, "#")); !fence && lvl > 0 && lvl <= 6 && strings.HasPrefix(t[lvl:], " ") {
			heads = append(heads, mdHeading{lvl, i + 1})
			stack = append(stack[:min(len(stack), lvl-1)], strings.TrimSpace(t[lvl:]))
			for len(stack) < lvl {
				stack = append([]string{""}, stack...)
			}
		}
		paths[i+1] = strings.Join(slices.DeleteFunc(slices.Clone(stack), func(x string) bool { return x == "" }), " > ")
	}
	return heads, paths
}

// notAuthorProse: comment-only blocks, rules (---), and blockquotes (quoted speech, callouts).
func notAuthorProse(u Unit) bool {
	t := strings.TrimSpace(htmlComment.ReplaceAllString(u.Text, ""))
	return t == "" || strings.Trim(t, "-*_ ") == "" || !slices.ContainsFunc(strings.Split(t, "\n"), func(l string) bool { return !strings.HasPrefix(strings.TrimSpace(l), ">") })
}

func nextLine(lines []string, from int) string {
	for j := from; j < len(lines); j++ {
		if t := strings.TrimSpace(lines[j]); t != "" {
			return t
		}
	}
	return ""
}

// sectionUnits: one unit per filled H2+ section, up to the next heading at its level or above.
// lines has code blanked, so a section that is only a fenced block reads as a placeholder: nothing to judge.
func sectionUnits(path string, lines []string, heads []mdHeading, header string) []Unit {
	var units []Unit
	for k, h := range heads {
		if h.level < 2 {
			continue
		}
		end := len(lines)
		if n := slices.IndexFunc(heads[k+1:], func(n mdHeading) bool { return n.level <= h.level }); n >= 0 {
			end = heads[k+1+n].line - 1
		}
		for end > h.line && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		if end > h.line && end-h.line < mdWholeLines && !placeholderBody(lines[h.line:end]) {
			units = append(units, Unit{Kind: "Section", File: path, Start: h.line, End: end, Text: strings.Join(lines[h.line-1:end], "\n"), Context: header})
		}
	}
	return units
}

// mdKind: a list starting "1." is Steps, so step rules skip plain bullets.
func mdKind(first string) string {
	switch {
	case !isBullet(first):
		return "Paragraph"
	case first[0] >= '0' && first[0] <= '9':
		return "Steps"
	}
	return "Bullet"
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// docHeader: the title and status every Markdown unit is judged against.
func docHeader(lines []string) string {
	title, status := "", ""
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for _, l := range lines[1:] {
			t := strings.TrimSpace(l)
			if t == "---" || t == "..." {
				break
			}
			if k, v, ok := strings.Cut(t, ":"); ok {
				switch strings.ToLower(k) {
				case "title":
					title = strings.Trim(strings.TrimSpace(v), `"'`)
				case "status":
					status = strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
		}
	}
	for _, l := range lines {
		if t := strings.TrimSpace(l); title == "" && strings.HasPrefix(t, "# ") {
			title = strings.TrimSpace(t[2:])
		}
	}
	h := "DOCUMENT TITLE: " + title
	if status != "" {
		h += "\nDOCUMENT STATUS: " + status
	}
	return h
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
			kw := strings.TrimSpace(strings.TrimPrefix(s, "}"))
			switch {
			case strings.HasPrefix(kw, "if "), strings.HasPrefix(kw, "if("),
				strings.HasPrefix(kw, "else "),
				strings.HasPrefix(kw, "switch "), strings.HasPrefix(kw, "switch("),
				strings.HasPrefix(kw, "for "), strings.HasPrefix(kw, "for("),
				strings.HasPrefix(kw, "while "), strings.HasPrefix(kw, "while("),
				strings.HasPrefix(kw, "try "), strings.HasPrefix(kw, "try{"), strings.HasPrefix(kw, "try:"),
				strings.HasPrefix(kw, "catch "), strings.HasPrefix(kw, "catch("), strings.HasPrefix(kw, "catch{"),
				strings.HasPrefix(kw, "except "), strings.HasPrefix(kw, "except:"),
				strings.HasPrefix(kw, "throw "), strings.HasPrefix(kw, "raise "),
				strings.HasPrefix(kw, "return "), strings.HasPrefix(kw, "defer "), strings.HasPrefix(kw, "go "),
				strings.Contains(s, "assert."), strings.Contains(s, "assert("), strings.Contains(s, "expect("),
				strings.Contains(s, "require."),
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

var (
	commentMarker = regexp.MustCompile(`^\s*(//+|#+|/\*+|\*+/?|--|;+)\s?`)
	ruleLine      = regexp.MustCompile(`^[^\p{L}\p{N}]{3,}$`)
	toolDirective = regexp.MustCompile(`^(shellcheck|eslint|prettier-ignore|@ts-|nolint|noqa|go:|lint:|lintdec:ignore|pylint|type: ?ignore|istanbul|c8 |biome-ignore|ktlint|tslint|jshint|NOSONAR|region|endregion)`)
)

// structuralComment: section banners and tool directives look like comments but say nothing to lint,
// and Jev flags them as tautologies.
func structuralComment(u Unit) bool {
	if u.Kind != "Comment" {
		return false
	}
	var rules, directives, titles int
	for _, line := range strings.Split(u.Text, "\n") {
		t := strings.TrimSpace(commentMarker.ReplaceAllString(line, ""))
		switch {
		case t == "" || ruleLine.MatchString(t):
			rules++
		case toolDirective.MatchString(t):
			directives++
		case len(strings.Fields(t)) <= 5:
			titles++
		default:
			return false
		}
	}
	return directives > 0 || (rules > 0 && titles <= 1 && strings.TrimSpace(u.Text) != "")
}

// placeholderLine: a line that only says the content is missing ("TBD", "N/A", "TODO: ABC-123").
var placeholderLine = regexp.MustCompile(`(?i)^[-*+_(\s]*(tbd|tba|todo|n/?a|wip|coming soon|to be (?:determined|added|written))\b[\s.:!)*_-]*(?:[a-z]+-\d+)?[\s.)*_]*$`)

// placeholderBody: an unfilled template section (comments, empty tables, sub-headings only) has nothing to judge yet.
func placeholderBody(body []string) bool {
	text := htmlComment.ReplaceAllString(strings.Join(body, "\n"), "")
	header := true
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.Trim(t, "-*+ ") == "", strings.HasPrefix(t, "#"), placeholderLine.MatchString(t), isCodePlaceholder(t): // blank, bare bullet, heading, TBD, or code block
		case strings.HasPrefix(t, "|"):
			if strings.Trim(t, "|-: ") == "" {
				header = false // separator row; rows after it are data
			} else if !header && strings.Trim(t, "| ") != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}
