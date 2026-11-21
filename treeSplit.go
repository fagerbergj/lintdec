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
	ifaces    map[string]bool // node types that map to Interface units
	callTests map[string]bool // call names that make the call a TestFunction
	testPref  string          // function-name prefix that makes a TestFunction
}

var tsLangs = map[string]tsCfg{
	"ts": {
		"typescript", tsx.GetLanguage,
		map[string]bool{"interface_declaration": true, "type_alias_declaration": true},
		map[string]bool{"describe": true, "it": true, "test": true, "xit": true, "xdescribe": true, "xtest": true, "beforeEach": true, "afterEach": true, "beforeAll": true, "afterAll": true},
		"",
	},
	"js": {
		"javascript", javascript.GetLanguage,
		nil,
		map[string]bool{"describe": true, "it": true, "test": true, "xit": true, "xdescribe": true, "xtest": true, "beforeEach": true, "afterEach": true, "beforeAll": true, "afterAll": true},
		"",
	},
	"py": {
		"python", python.GetLanguage,
		nil, nil, "test_",
	},
	"rs": {
		"rust", rust.GetLanguage,
		map[string]bool{"trait_definition": true},
		nil, "test_",
	},
	"sh": {
		"bash", bash.GetLanguage,
		nil, nil, "",
	},
	"java": {
		"java", java.GetLanguage,
		map[string]bool{"interface_declaration": true},
		nil, "test_",
	},
	"c": {
		"c", c.GetLanguage,
		nil, nil, "",
	},
	"cpp": {
		"cpp", cpp.GetLanguage,
		nil, nil, "",
	},
	"cs": {
		"csharp", csharp.GetLanguage,
		map[string]bool{"interface_declaration": true},
		nil, "",
	},
	"kt": {
		"kotlin", kotlin.GetLanguage,
		map[string]bool{"interface_declaration": true},
		nil, "",
	},
	"swift": {
		"swift", swift.GetLanguage,
		nil, nil, "",
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
	docs := map[*sitter.Node]string{}

	funcUnit := func(n *sitter.Node, kind string) Unit {
		u := Unit{Kind: kind, File: path,
			Start: int(n.StartPoint().Row) + 1, End: int(n.EndPoint().Row) + 1,
			Text: n.Content(src)}
		if d, ok := docs[n.PrevSibling()]; ok {
			u.Context = d
		} else if d := pythonDocstring(n, src); d != "" {
			u.Context = d
		}
		return u
	}

	// Sibling-order traversal so a doc comment is seen before its function.
	// inCovered marks arrows inside an already-emitted const declaration.
	var walk func(n *sitter.Node, inCovered bool)
	walk = func(n *sitter.Node, inCovered bool) {
		for i, count := 0, int(n.ChildCount()); i < count; i++ {
			c := n.Child(i)
			t := c.Type()
			if strings.Contains(t, "comment") {
				doc := docRun(c, src)
				if next := c.NextSibling(); next != nil && isUnitStart(next, cfg, src) &&
					c.EndPoint().Row+1 == next.StartPoint().Row {
					docs[c] = doc // attach below, not emitted as a unit
				} else {
					units = append(units, Unit{Kind: "Comment", File: path,
						Start: int(c.StartPoint().Row) + 1, End: int(c.EndPoint().Row) + 1,
						Text: strings.Join(lines[int(c.StartPoint().Row):int(c.EndPoint().Row)+1], "\n")})
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
						continue // the wrapper is the unit; nested decls inside are rare
					}
				}
				walk(c, inCovered)
				continue
			}
			switch {
			case kindForNode(c, cfg, src) != "":
				if t == "call_expression" {
					units = append(units, funcUnit(c, kindForNode(c, cfg, src)))
					continue // test body is part of the unit; no nested units
				}
				units = append(units, funcUnit(c, kindForNode(c, cfg, src)))
				if t == "lexical_declaration" {
					walk(c, true)
				}
			default:
				walk(c, inCovered)
			}
		}
	}
	walk(root, false)

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
	if t == "call_expression" && cfg.callTests[calleeName(n, src)] {
		return "TestFunction"
	}
	if t == "lexical_declaration" && hasFnValue(n) {
		return "Function"
	}
	return ""
}

// hasFnValue: does a lexical_declaration declare a function or arrow?
func hasFnValue(n *sitter.Node) bool {
	var check func(m *sitter.Node, depth int) bool
	check = func(m *sitter.Node, depth int) bool {
		if depth > 3 {
			return false
		}
		for i, count := 0, int(m.NamedChildCount()); i < count; i++ {
			c := m.NamedChild(i)
			if c.Type() == "arrow_function" || c.Type() == "function_expression" {
				return true
			}
			if check(c, depth+1) {
				return true
			}
		}
		return false
	}
	return check(n, 0)
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
func docRun(n *sitter.Node, src []byte) string {
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
	return strings.TrimSpace(b.String())
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
