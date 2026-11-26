package main

// Generic tree-sitter splitter for every code language except Go
// (Go stays on go/ast for exact comment attachment). Produces the same
// unit kinds as splitGo so all rules and the cache work unchanged.

import (
	"context"
	"fmt"
	"os"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/bash"
	"github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/kotlin"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
)

type tsCfg struct {
	name      string
	lang      func() *sitter.Language
	ifaces    map[string]bool
	callTests map[string]bool
	callHooks map[string]bool
	testPref  string
}

var (
	jsTestCalls = map[string]bool{"it": true, "test": true, "xit": true, "xtest": true, "fit": true, "ftest": true}
	jsHookCalls = map[string]bool{"before": true, "after": true, "beforeEach": true, "afterEach": true, "beforeAll": true, "afterAll": true}
)

var tsLangs = map[string]tsCfg{
	"ts": {
		"typescript", tsx.GetLanguage,
		map[string]bool{"interface_declaration": true},
		jsTestCalls, jsHookCalls,
		"",
	},
	"js": {
		"javascript", javascript.GetLanguage,
		nil,
		jsTestCalls, jsHookCalls,
		"",
	},
	"py": {
		"python", python.GetLanguage,
		nil, nil, nil, "test_",
	},
	"rs": {
		"rust", rust.GetLanguage,
		map[string]bool{"trait_definition": true},
		nil, nil, "test_",
	},
	"sh": {
		"bash", bash.GetLanguage,
		nil, nil, nil, "",
	},
	"java": {
		"java", java.GetLanguage,
		map[string]bool{"interface_declaration": true},
		nil, nil, "test_",
	},
	"c": {
		"c", c.GetLanguage,
		nil, nil, nil, "",
	},
	"cpp": {
		"cpp", cpp.GetLanguage,
		nil, nil, nil, "",
	},
	"cs": {
		"csharp", csharp.GetLanguage,
		map[string]bool{"interface_declaration": true},
		nil, nil, "",
	},
	"kt": {
		"kotlin", kotlin.GetLanguage,
		map[string]bool{"interface_declaration": true},
		nil, nil, "",
	},
	"swift": {
		"swift", swift.GetLanguage,
		nil, nil, nil, "",
	},
}

// tsExt maps file extensions to the tsLangs key.
var tsExt = map[string]string{
	".ts": "ts", ".tsx": "ts", ".mts": "ts", ".cts": "ts",
	".js": "js", ".jsx": "js", ".mjs": "js", ".cjs": "js",
	".py": "py", ".rs": "rs", ".sh": "sh", ".bash": "sh",
	".java": "java", ".c": "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp",
	".cs": "cs", ".kt": "kt", ".kts": "kt", ".swift": "swift",
}

var tsFuncKinds = map[string]bool{
	"function_declaration": true, "method_definition": true, "function_item": true,
	"function_definition": true, "constructor_declaration": true,
}

func splitTree(path string, src []byte, cfg tsCfg) []Unit {
	root, err := sitter.ParseCtx(context.Background(), src, cfg.lang())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: %s (%s): %v (skipped)\n", path, cfg.name, err)
		return nil
	}
	lines := strings.Split(string(src), "\n")
	var units []Unit
	type docInfo struct {
		text  string
		start int
	}
	docs := map[*sitter.Node]docInfo{}

	funcUnit := func(n *sitter.Node, kind string) Unit {
		u := Unit{Kind: kind, File: path,
			Start: int(n.StartPoint().Row) + 1, End: int(n.EndPoint().Row) + 1,
			Text: n.Content(src), name: unitDeclName(n, src)}
		if d, ok := docs[n.PrevSibling()]; ok {
			u.Context, u.ContextStart = d.text, d.start
		} else if d := pythonDocstring(n, src); d != "" {
			u.Context = d
		}
		return u
	}

	// Sibling-order traversal attaches doc comments to declarations; comments are
	// collected in a full-tree pass below so doc and inline comments match splitGo.
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		for i, count := 0, int(n.ChildCount()); i < count; i++ {
			c := n.Child(i)
			t := c.Type()
			if strings.Contains(t, "comment") {
				if attachesDoc(c, cfg, src) {
					text, start := docRun(c, src)
					docs[c] = docInfo{text: text, start: start}
				}
				continue
			}
			if !c.IsNamed() {
				continue
			}
			// export_statement wraps the real declaration; unit the wrapper so
			// the span keeps the export and the doc comment attaches.
			if t == "export_statement" {
				if inner := innerDecl(c); inner != nil {
					if kind := kindForNode(inner, cfg, src); kind != "" {
						units = append(units, funcUnit(c, kind))
						continue
					}
				}
				walk(c)
				continue
			}
			if t == "call_expression" && isHookCall(calleeName(c, src), cfg) {
				continue
			}
			if kind := kindForNode(c, cfg, src); kind != "" {
				units = append(units, funcUnit(c, kind))
				continue
			}
			walk(c)
		}
	}
	walk(root)
	units = append(units, collectComments(root, path, cfg, src, lines)...)

	// File skeleton from top-level declarations.
	var sk []string
	sk = append(sk, fmt.Sprintf("file %s (lines 1-%d)", path, len(lines)))
	for i, count := 0, int(root.ChildCount()); i < count; i++ {
		child := root.Child(i)
		if !child.IsNamed() || strings.Contains(child.Type(), "comment") {
			continue
		}
		target := child
		if child.Type() == "export_statement" {
			if inner := innerDecl(child); inner != nil {
				target = inner
			}
		}
		if name := declName(target, src); name != "" {
			sk = append(sk, fmt.Sprintf("%s %s (%d-%d)", target.Type(), name,
				child.StartPoint().Row+1, child.EndPoint().Row+1))
			if len(sk) > 61 {
				break
			}
		}
	}
	if len(sk) > 1 {
		units = append(units, Unit{Kind: "File", File: path, Start: 1, End: len(lines),
			Text: strings.Join(sk, "\n")})
	}
	return units
}

// innerDecl: the declaration wrapped by an export_statement.
func innerDecl(n *sitter.Node) *sitter.Node {
	for i, count := 0, int(n.ChildCount()); i < count; i++ {
		c := n.Child(i)
		if c.IsNamed() {
			return c
		}
	}
	return nil
}

// isUnitStart: nodes that begin a lintable unit (doc comments attach to these).
func isUnitStart(n *sitter.Node, cfg tsCfg, src []byte) bool {
	if n.Type() == "export_statement" {
		if inner := innerDecl(n); inner != nil {
			return kindForNode(inner, cfg, src) != ""
		}
		return false
	}
	return kindForNode(n, cfg, src) != ""
}

// attachesDoc reports whether c sits on the line immediately above a lintable sibling unit.
func attachesDoc(c *sitter.Node, cfg tsCfg, src []byte) bool {
	next := c.NextSibling()
	return next != nil && isUnitStart(next, cfg, src) && c.EndPoint().Row+1 == next.StartPoint().Row
}

// collectComments walks the whole tree and groups contiguous full-line comment
// runs into Comment units with surrounding declaration or +-8 line context.
func collectComments(root *sitter.Node, path string, cfg tsCfg, src []byte, lines []string) []Unit {
	var nodes []*sitter.Node
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		if strings.Contains(n.Type(), "comment") {
			txt := n.Content(src)
			if !(n.StartPoint().Row == 0 && strings.HasPrefix(txt, "#!")) {
				nodes = append(nodes, n)
			}
			return
		}
		for i, count := 0, int(n.ChildCount()); i < count; i++ {
			visit(n.Child(i))
		}
	}
	visit(root)

	var out []Unit
	for i := 0; i < len(nodes); {
		first, last := nodes[i], nodes[i]
		parts := []string{first.Content(src)}
		j := i + 1
		for j < len(nodes) && canMergeComment(last, nodes[j], lines, src) {
			last = nodes[j]
			parts = append(parts, last.Content(src))
			j++
		}
		s, e := int(first.StartPoint().Row)+1, int(last.EndPoint().Row)+1
		fullLine := isCommentOnlyLine(lines, int(first.StartPoint().Row), int(first.StartPoint().Column))
		out = append(out, Unit{
			Kind: "Comment", File: path, Start: s, End: e,
			Text:    strings.Join(parts, "\n"),
			Context: treeCommentContext(lines, last, s, e, cfg, src, fullLine),
		})
		i = j
	}
	return out
}

// treeCommentContext mirrors goCommentContext: a full-line comment directly above
// or any comment inside a bounded declaration sees that declaration alone.
func treeCommentContext(lines []string, last *sitter.Node, s, e int, cfg tsCfg, src []byte, fullLine bool) string {
	nxt := last.NextSibling()
	if nxt != nil && fullLine && isUnitStart(nxt, cfg, src) && int(nxt.StartPoint().Row)+1 == e+1 {
		if de := min(int(nxt.EndPoint().Row)+1, len(lines)); de-s <= rawBudgetLines {
			return numberedContext(lines, e+1, de, s, e)
		}
	}
	for p := last.Parent(); p != nil; p = p.Parent() {
		if !isUnitStart(p, cfg, src) {
			continue
		}
		ds, de := int(p.StartPoint().Row)+1, min(int(p.EndPoint().Row)+1, len(lines))
		if de-ds <= rawBudgetLines {
			if ctx := numberedContext(lines, ds, de, s, e); ctx != "" {
				return ctx
			}
		}
		break
	}
	if nxt != nil && fullLine && int(nxt.StartPoint().Row)+1 == e+1 {
		if de := min(int(nxt.EndPoint().Row)+1, len(lines)); de-s <= rawBudgetLines {
			return numberedContext(lines, e+1, de, s, e)
		}
	}
	return commentContext(lines, s, e)
}

func canMergeComment(a, b *sitter.Node, lines []string, src []byte) bool {
	if b.StartPoint().Row != a.EndPoint().Row+1 || b.StartPoint().Column != a.StartPoint().Column {
		return false
	}
	if isDirectiveComment(a, src) || isDirectiveComment(b, src) {
		return false
	}
	return isCommentOnlyLine(lines, int(a.StartPoint().Row), int(a.StartPoint().Column)) &&
		isCommentOnlyLine(lines, int(b.StartPoint().Row), int(b.StartPoint().Column))
}

func isDirectiveComment(n *sitter.Node, src []byte) bool {
	t := strings.TrimSpace(commentMarker.ReplaceAllString(n.Content(src), ""))
	return toolDirective.MatchString(t)
}

func isCommentOnlyLine(lines []string, row, col int) bool {
	if row < 0 || row >= len(lines) || col > len(lines[row]) {
		return false
	}
	return strings.TrimSpace(lines[row][:col]) == ""
}

// commentContext: +-8 non-blank surrounding lines with line numbers.
func commentContext(lines []string, s, e int) string {
	return numberedContext(lines, s-8, e+8, s, e)
}

// numberedContext: non-blank lines from..to with line numbers, skipping the target s..e.
func numberedContext(lines []string, from, to, s, e int) string {
	var ctx []string
	for ln := from; ln <= to; ln++ {
		if ln < 1 || ln > len(lines) || (ln >= s && ln <= e) {
			continue
		}
		if t := strings.TrimSpace(lines[ln-1]); t != "" {
			ctx = append(ctx, fmt.Sprintf("%d\t%s", ln, lines[ln-1]))
		}
	}
	return strings.Join(ctx, "\n")
}

func isTestCall(callee string, cfg tsCfg) bool {
	if len(cfg.callTests) == 0 {
		return false
	}
	if cfg.callTests[callee] {
		return true
	}
	base, prop, ok := strings.Cut(callee, ".")
	return ok && cfg.callTests[base] && prop != "todo"
}

func isHookCall(callee string, cfg tsCfg) bool {
	return len(cfg.callHooks) > 0 && cfg.callHooks[callee]
}

func kindForNode(n *sitter.Node, cfg tsCfg, src []byte) string {
	t := n.Type()
	if cfg.ifaces[t] {
		return "Interface"
	}
	if tsFuncKinds[t] {
		if cfg.testPref != "" && strings.HasPrefix(funcName(n, src), cfg.testPref) {
			return "TestFunction"
		}
		return "Function"
	}
	if t == "call_expression" && isTestCall(calleeName(n, src), cfg) {
		return "TestFunction"
	}
	if t == "lexical_declaration" && hasFnValue(n) {
		return "Function"
	}
	return ""
}

// hasFnValue reports whether a lexical_declaration directly initializes a
// variable to a function or arrow expression (not a call/table containing callbacks).
func hasFnValue(n *sitter.Node) bool {
	for i, count := 0, int(n.NamedChildCount()); i < count; i++ {
		c := n.NamedChild(i)
		if c.Type() == "variable_declarator" && isFnExpr(c.ChildByFieldName("value")) {
			return true
		}
	}
	return false
}

func isFnExpr(n *sitter.Node) bool {
	for n != nil {
		switch n.Type() {
		case "arrow_function", "function_expression":
			return true
		case "parenthesized_expression", "as_expression", "satisfies_expression":
			if n.NamedChildCount() == 0 {
				return false
			}
			n = n.NamedChild(0)
		default:
			return false
		}
	}
	return false
}

// unitDeclName returns the declared name of a top-level function/variable unit,
// or "" for methods/constructors that are only reachable through a receiver.
func unitDeclName(n *sitter.Node, src []byte) string {
	if n.Type() == "export_statement" {
		if inner := innerDecl(n); inner != nil {
			n = inner
		}
	}
	switch n.Type() {
	case "method_definition", "method_declaration", "constructor_declaration":
		return ""
	case "lexical_declaration":
		for i, count := 0, int(n.NamedChildCount()); i < count; i++ {
			c := n.NamedChild(i)
			if c.Type() == "variable_declarator" && isFnExpr(c.ChildByFieldName("value")) {
				if id := c.ChildByFieldName("name"); id != nil {
					return id.Content(src)
				}
			}
		}
		return ""
	}
	if id := n.ChildByFieldName("name"); id != nil {
		return id.Content(src)
	}
	if decl := n.ChildByFieldName("declarator"); decl != nil {
		return funcName(decl, src)
	}
	return funcName(n, src)
}

// funcName returns the declared identifier of a function-like node.
func funcName(n *sitter.Node, src []byte) string {
	for i, count := 0, int(n.ChildCount()); i < count; i++ {
		c := n.Child(i)
		if c.IsNamed() && (c.Type() == "identifier" || c.Type() == "property_identifier" || c.Type() == "type_identifier") {
			return c.Content(src)
		}
	}
	return ""
}

// calleeName returns the callee text of a call_expression.
func calleeName(n *sitter.Node, src []byte) string {
	if n.ChildCount() == 0 {
		return ""
	}
	return n.Child(0).Content(src)
}

// declName: identifier for the file skeleton.
func declName(n *sitter.Node, src []byte) string {
	if n.Type() == "call_expression" {
		return ""
	}
	for i, count := 0, int(n.ChildCount()); i < count; i++ {
		c := n.Child(i)
		if c.IsNamed() && (c.Type() == "identifier" || c.Type() == "type_identifier") {
			return c.Content(src)
		}
	}
	return ""
}

// docRun: maximal run of consecutive comment siblings ending at n (n last),
// doc markers stripped.
func docRun(n *sitter.Node, src []byte) (string, int) {
	run := []*sitter.Node{n}
	cur := n
	for {
		prev := cur.PrevSibling()
		if prev == nil || !strings.Contains(prev.Type(), "comment") {
			break
		}
		if cur.StartPoint().Row == 0 || prev.EndPoint().Row+1 < cur.StartPoint().Row {
			break
		}
		run = append([]*sitter.Node{prev}, run...)
		cur = prev
	}
	var b strings.Builder
	for _, c := range run {
		line := strings.TrimSpace(c.Content(src))
		line = strings.TrimPrefix(line, "/**")
		line = strings.TrimSuffix(line, "*/")
		line = strings.TrimPrefix(line, "*")
		line = strings.TrimPrefix(line, "//")
		line = strings.TrimPrefix(line, "///")
		line = strings.TrimPrefix(line, "//!")
		line = strings.TrimPrefix(line, "#")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String()), int(cur.StartPoint().Row) + 1
}

// pythonDocstring: first string literal in a python function body.
func pythonDocstring(n *sitter.Node, src []byte) string {
	if n.Type() != "function_definition" {
		return ""
	}
	body := n.ChildByFieldName("body")
	if body == nil || body.ChildCount() == 0 {
		return ""
	}
	first := body.Child(0)
	if first.Type() != "expression_statement" || first.ChildCount() == 0 {
		return ""
	}
	s := first.Child(0)
	if s.Type() != "string" {
		return ""
	}
	text := strings.ReplaceAll(s.Content(src), `"""`, "")
	text = strings.ReplaceAll(text, `'''`, "")
	return strings.TrimSpace(text)
}
