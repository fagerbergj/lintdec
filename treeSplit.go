package main

// Generic tree-sitter splitter for every code language except Go
// (Go stays on go/ast for exact comment attachment). Produces the same
// unit kinds as splitGo so all rules and the cache work unchanged.

import (
	"context"
	"fmt"
	"os"
	"path"
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
	bodyUnits bool
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
		"", false,
	},
	"js": {
		"javascript", javascript.GetLanguage,
		nil,
		jsTestCalls, jsHookCalls,
		"", false,
	},
	"py": {
		"python", python.GetLanguage,
		nil, nil, nil, "test_", false,
	},
	"rs": {
		"rust", rust.GetLanguage,
		map[string]bool{"trait_definition": true},
		nil, nil, "test_", false,
	},
	"sh": {
		"bash", bash.GetLanguage,
		nil, nil, nil, "", true,
	},
	"java": {
		"java", java.GetLanguage,
		map[string]bool{"interface_declaration": true},
		nil, nil, "test_", false,
	},
	"c": {
		"c", c.GetLanguage,
		nil, nil, nil, "", false,
	},
	"cpp": {
		"cpp", cpp.GetLanguage,
		nil, nil, nil, "", false,
	},
	"cs": {
		"csharp", csharp.GetLanguage,
		map[string]bool{"interface_declaration": true},
		nil, nil, "", false,
	},
	"kt": {
		"kotlin", kotlin.GetLanguage,
		map[string]bool{"interface_declaration": true},
		nil, nil, "", false,
	},
	"swift": {
		"swift", swift.GetLanguage,
		nil, nil, nil, "", false,
	},
}

// tsExt maps file extensions to the tsLangs key.
var tsExt = map[string]string{
	".ts": "ts", ".tsx": "ts", ".mts": "ts", ".cts": "ts",
	".js": "js", ".jsx": "js", ".mjs": "js", ".cjs": "js",
	".py": "py", ".rs": "rs", ".sh": "sh", ".bash": "sh", ".zsh": "sh",
	".java": "java", ".c": "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp",
	".cs": "cs", ".kt": "kt", ".kts": "kt", ".swift": "swift",
}

var tsFuncKinds = map[string]bool{
	"function_declaration": true, "method_definition": true, "function_item": true,
	"function_definition": true, "constructor_declaration": true,
}

func splitTree(filePath string, src []byte, cfg tsCfg) []Unit {
	root, err := sitter.ParseCtx(context.Background(), src, cfg.lang())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: %s (%s): %v (skipped)\n", filePath, cfg.name, err)
		return nil
	}
	lines := strings.Split(string(src), "\n")
	var units []Unit
	var hooks []testHook
	type docInfo struct {
		text  string
		start int
	}
	docs := map[*sitter.Node]docInfo{}

	funcUnit := func(docNode, textNode, declNode *sitter.Node, kind string) Unit {
		u := Unit{Kind: kind, File: filePath,
			Start: int(textNode.StartPoint().Row) + 1, End: int(textNode.EndPoint().Row) + 1,
			Text: textNode.Content(src), name: unitDeclName(declNode, src)}
		if d, ok := docs[docNode.PrevSibling()]; ok {
			u.Context, u.ContextStart = d.text, d.start
		} else if d := pythonDocstring(declNode, src); d != "" {
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
			if hookNode := unwrapHookCall(c, cfg, src); hookNode != nil {
				hooks = append(hooks, makeTestHook(n, c, hookNode, src))
				continue
			}
			if textNode, declNode, kind := unwrapUnitNode(c, cfg, src); kind != "" {
				units = append(units, funcUnit(c, textNode, declNode, kind))
				continue
			}
			walk(c)
		}
	}
	walk(root)
	attachTestHooks(units, hooks)
	if cfg.bodyUnits {
		units = append(units, shellBodyUnits(root, filePath, src, lines)...)
	}
	units = append(units, collectComments(root, filePath, cfg, src, lines)...)

	// File skeleton from top-level declarations.
	var sk []string
	sk = append(sk, fmt.Sprintf("file %s (lines 1-%d)", filePath, len(lines)))
	for i, count := 0, int(root.ChildCount()); i < count; i++ {
		child := root.Child(i)
		if !child.IsNamed() || strings.Contains(child.Type(), "comment") {
			continue
		}
		target := unwrapSkeletonNode(child)
		if name := declName(target, src); name != "" {
			sk = append(sk, fmt.Sprintf("%s %s (%d-%d)", target.Type(), name,
				child.StartPoint().Row+1, child.EndPoint().Row+1))
			if len(sk) > 61 {
				break
			}
		}
	}
	if len(sk) > 1 {
		units = append(units, Unit{Kind: "File", File: filePath, Start: 1, End: len(lines),
			Text: strings.Join(sk, "\n")})
	}
	return units
}

var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true,
}

// isShellShebang reports whether src opens with a POSIX/bash/zsh shebang line.
func isShellShebang(src []byte) bool {
	if len(src) < 3 || src[0] != '#' || src[1] != '!' {
		return false
	}
	line := string(src[:min(len(src), 128)])
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	fields := strings.Fields(strings.TrimRight(line[2:], "\r"))
	if len(fields) == 0 {
		return false
	}
	cmd := path.Base(fields[0])
	if cmd == "env" {
		for _, arg := range fields[1:] {
			if strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
				continue
			}
			cmd = path.Base(arg)
			break
		}
	}
	return shellInterpreters[cmd]
}

func hasShellShebangFile(filePath string) bool {
	if st, err := os.Stat(filePath); err != nil || !st.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [128]byte
	n, _ := f.Read(buf[:])
	return isShellShebang(buf[:n])
}

// declaresFunc reports whether n is or contains a function_definition.
func declaresFunc(n *sitter.Node) bool {
	if n.Type() == "function_definition" {
		return true
	}
	for i, count := 0, int(n.ChildCount()); i < count; i++ {
		if declaresFunc(n.Child(i)) {
			return true
		}
	}
	return false
}

// shellRunDoc returns the comment run immediately above first, skipping a line-1 shebang.
func shellRunDoc(first *sitter.Node, src []byte) (string, int) {
	prev := first.PrevSibling()
	if prev == nil || !strings.Contains(prev.Type(), "comment") || prev.EndPoint().Row+1 < first.StartPoint().Row {
		return "", 0
	}
	if prev.StartPoint().Row == 0 && strings.HasPrefix(prev.Content(src), "#!") {
		return "", 0
	}
	text, start := docRun(prev, src)
	if start == 1 && strings.HasPrefix(text, "!") {
		if nl := strings.IndexByte(text, '\n'); nl >= 0 {
			return strings.TrimSpace(text[nl+1:]), 2
		}
		return "", 0
	}
	return text, start
}

// shellBodyUnits emits Script units for top-level executable runs outside
// function definitions, keeping them disjoint from Function units.
func shellBodyUnits(root *sitter.Node, filePath string, src []byte, lines []string) []Unit {
	var units []Unit
	var first *sitter.Node
	start, end, hasExec := 0, 0, false
	flush := func() {
		if hasExec && start > 0 && end >= start {
			e := min(end, len(lines))
			u := Unit{
				Kind: "Script", File: filePath, Start: start, End: e,
				Text: strings.Join(lines[start-1:e], "\n"),
			}
			if first != nil {
				u.Context, u.ContextStart = shellRunDoc(first, src)
			}
			units = append(units, u)
		}
		first, start, end, hasExec = nil, 0, 0, false
	}
	for i, count := 0, int(root.ChildCount()); i < count; i++ {
		c := root.Child(i)
		if !c.IsNamed() || strings.Contains(c.Type(), "comment") {
			continue
		}
		if declaresFunc(c) {
			flush()
			continue
		}
		s, e := int(c.StartPoint().Row)+1, int(c.EndPoint().Row)+1
		if start == 0 {
			first, start = c, s
		}
		end = e
		if c.Type() != "variable_assignment" && c.Type() != "declaration_command" {
			hasExec = true
		}
	}
	flush()
	return units
}

// innerDecl: the declaration wrapped by an export_statement or expression_statement.
func innerDecl(n *sitter.Node) *sitter.Node {
	for i, count := 0, int(n.ChildCount()); i < count; i++ {
		c := n.Child(i)
		if c.IsNamed() {
			return c
		}
	}
	return nil
}

// unwrapUnitNode returns (textNode, declNode, kind) for a lintable unit or wrapper,
// keeping exports/decorators in textNode and unwrapping expression_statement test calls.
func unwrapUnitNode(n *sitter.Node, cfg tsCfg, src []byte) (*sitter.Node, *sitter.Node, string) {
	switch n.Type() {
	case "export_statement":
		if inner := innerDecl(n); inner != nil {
			if kind := kindForNode(inner, cfg, src); kind != "" {
				return n, inner, kind
			}
		}
		return nil, nil, ""
	case "decorated_definition":
		if inner := n.ChildByFieldName("definition"); inner != nil {
			if kind := kindForNode(inner, cfg, src); kind != "" {
				return n, inner, kind
			}
		}
		return nil, nil, ""
	case "expression_statement":
		if inner := innerDecl(n); inner != nil && inner.Type() == "call_expression" {
			if kind := kindForNode(inner, cfg, src); kind == "TestFunction" {
				return inner, inner, kind
			}
		}
		return nil, nil, ""
	}
	if kind := kindForNode(n, cfg, src); kind != "" {
		return n, n, kind
	}
	return nil, nil, ""
}

func unwrapHookCall(n *sitter.Node, cfg tsCfg, src []byte) *sitter.Node {
	target := n
	if n.Type() == "expression_statement" {
		target = innerDecl(n)
	}
	if target != nil && target.Type() == "call_expression" && isHookCall(calleeName(target, src), cfg) {
		return target
	}
	return nil
}

func unwrapSkeletonNode(n *sitter.Node) *sitter.Node {
	switch n.Type() {
	case "export_statement":
		if inner := innerDecl(n); inner != nil {
			return inner
		}
	case "decorated_definition":
		if inner := n.ChildByFieldName("definition"); inner != nil {
			return inner
		}
	}
	return n
}

const (
	maxTestHookLines = 60
	maxTestHookTotal = 80
)

type testHook struct {
	scopeStart int
	scopeEnd   int
	lines      int
	text       string
}

func makeTestHook(parent, stmt, hook *sitter.Node, src []byte) testHook {
	start := int(hook.StartPoint().Row) + 1
	rawLines := strings.Split(hook.Content(src), "\n")
	if len(rawLines) > maxTestHookLines {
		rawLines = append(rawLines[:maxTestHookLines], "...")
	}
	text := numberedLines(strings.Join(rawLines, "\n"), start)
	lineCount := len(rawLines)
	if prev := stmt.PrevSibling(); prev != nil && strings.Contains(prev.Type(), "comment") && prev.EndPoint().Row+1 == stmt.StartPoint().Row {
		if doc, docStart := docRun(prev, src); doc != "" {
			docLines := strings.Split(doc, "\n")
			text = numberedLines(doc, docStart) + "\n" + text
			lineCount += len(docLines)
		}
	}
	return testHook{
		scopeStart: int(parent.StartPoint().Row) + 1,
		scopeEnd:   int(parent.EndPoint().Row) + 1,
		lines:      lineCount,
		text:       text,
	}
}

// attachTestHooks appends enclosing suite lifecycle hooks (afterEach/beforeEach) to TestFunction Context.
func attachTestHooks(units []Unit, hooks []testHook) {
	if len(hooks) == 0 {
		return
	}
	for i := range units {
		if units[i].Kind != "TestFunction" {
			continue
		}
		var refs []string
		used := 0
		for _, h := range hooks {
			if h.scopeStart > units[i].Start || units[i].End > h.scopeEnd {
				continue
			}
			if used+h.lines > maxTestHookTotal {
				continue
			}
			refs = append(refs, h.text)
			used += h.lines
		}
		if len(refs) == 0 {
			continue
		}
		block := "LIFECYCLE HOOKS IN THIS FILE:\n" + strings.Join(refs, "\n\n")
		if units[i].Context != "" {
			units[i].Context = units[i].Context + "\n\n" + block
		} else {
			units[i].Context = block
		}
	}
}

// isUnitStart: nodes that begin a lintable unit (doc comments attach to these).
func isUnitStart(n *sitter.Node, cfg tsCfg, src []byte) bool {
	_, _, kind := unwrapUnitNode(n, cfg, src)
	return kind != ""
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

// treeCommentContext mirrors goCommentContext; directives keep +-8 lines above
// and below so preceding rationale comments stay visible to suppression-no-reason.
func treeCommentContext(lines []string, last *sitter.Node, s, e int, cfg tsCfg, src []byte, fullLine bool) string {
	if isDirectiveComment(last, src) {
		return commentContext(lines, s, e)
	}
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
	return isToolDirective(t)
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
	if len(cfg.callHooks) == 0 {
		return false
	}
	if cfg.callHooks[callee] {
		return true
	}
	base, prop, ok := strings.Cut(callee, ".")
	return ok && (base == "test" || base == "describe") && cfg.callHooks[prop]
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
