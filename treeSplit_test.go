package main

import (
	"strings"
	"testing"
)

const probeSrc = `import { readdirSync } from 'node:fs';
/** doc for arrowProblem */
export function arrowProblem(from: string, to: string): string | undefined {
  if (from === to) return undefined;
  return 'x';
}
type Edge = { from: string; to: string };
export interface Graph { nodes: number; }
export const process = (a: string) => a.length;
`

func TestSplitTreeTS(t *testing.T) {
	units := splitTree("probe.ts", []byte(probeSrc), tsLangs["ts"])
	var kinds []string
	var arrow *Unit
	for i := range units {
		kinds = append(kinds, units[i].Kind)
		if units[i].Start == 3 {
			arrow = &units[i]
		}
	}
	joined := strings.Join(kinds, ",")
	for _, want := range []string{"Function", "Interface", "File"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s unit in %s", want, joined)
		}
	}
	if n := countKind(units, "Function"); n != 2 {
		t.Errorf("want 2 Function units (export fn + const arrow), got %d in %s", n, joined)
	}
	// only interface_declaration is an Interface; the type alias Edge is not
	if n := countKind(units, "Interface"); n != 1 {
		t.Errorf("want 1 Interface unit (interface only, not type alias), got %d in %s", n, joined)
	}
	// doc comment must attach to the exported function
	if arrow == nil || arrow.Kind != "Function" || arrow.Context != "doc for arrowProblem" {
		t.Errorf("exported function lost its doc: %+v", arrow)
	}
	if !strings.Contains(arrow.Text, "export function arrowProblem") {
		t.Errorf("unit text must include the export line, got: %.60q", arrow.Text)
	}
}

func TestSplitTreePython(t *testing.T) {
	src := `"""Module doc."""

# standalone comment


def test_add(a, b):
    """Adds a and b."""
    return a + b


def helper(x):
    return x * 2
`
	units := splitTree("probe.py", []byte(src), tsLangs["py"])
	byKind := map[string][]Unit{}
	for _, u := range units {
		byKind[u.Kind] = append(byKind[u.Kind], u)
	}
	if len(byKind["TestFunction"]) != 1 {
		t.Fatalf("want 1 TestFunction, got %d", len(byKind["TestFunction"]))
	}
	tf := byKind["TestFunction"][0]
	if tf.Context != "Adds a and b." {
		t.Errorf("docstring not attached: %q", tf.Context)
	}
	if len(byKind["Function"]) != 1 || !strings.Contains(byKind["Function"][0].Text, "def helper") {
		t.Errorf("helper function unit wrong: %+v", byKind["Function"])
	}
	if len(byKind["Comment"]) != 1 || !strings.Contains(byKind["Comment"][0].Text, "standalone") {
		t.Errorf("standalone comment unit wrong: %+v", byKind["Comment"])
	}
}

func TestSplitTreeDocGapAndFields(t *testing.T) {
	tsSrc := `// unrelated header comment

// doc for greet
export function greet(name: string) {
  return name;
}
`
	units := splitTree("gap.ts", []byte(tsSrc), tsLangs["ts"])
	for _, u := range units {
		if u.Kind == "Function" {
			if strings.Contains(u.Context, "unrelated") {
				t.Errorf("blank-line-separated comment leaked into doc context: %q", u.Context)
			}
			if !strings.Contains(u.Context, "doc for greet") {
				t.Errorf("adjacent doc comment missing from context: %q", u.Context)
			}
		}
	}

	javaSrc := `class Box {
  int width;
  int height;
  int area() { return width * height; }
}
`
	jUnits := splitTree("Box.java", []byte(javaSrc), tsLangs["java"])
	if n := countKind(jUnits, "Function"); n != 0 {
		t.Errorf("java fields should not be emitted as Function units, got %d", n)
	}
}

func TestSplitTreeCommentGrouping(t *testing.T) {
	bashSrc := `set -euo pipefail

# first line of a note
# second line of a note

main() {
  echo hello
}
`
	units := splitTree("probe.sh", []byte(bashSrc), tsLangs["sh"])
	var cm *Unit
	for i := range units {
		if units[i].Kind == "Comment" {
			if cm != nil {
				t.Fatalf("want 1 Comment unit (grouped run), got >1: %+v", units)
			}
			cm = &units[i]
		}
	}
	if cm == nil {
		t.Fatal("no Comment unit emitted")
	}
	if !strings.Contains(cm.Text, "first line of a note") || !strings.Contains(cm.Text, "second line of a note") {
		t.Errorf("grouped comment text missing run: %q", cm.Text)
	}
	if !strings.Contains(cm.Context, "set -euo pipefail") || !strings.Contains(cm.Context, "echo hello") {
		t.Errorf("comment context missing surrounding code: %q", cm.Context)
	}
}

func TestSplitTreeTSInterfaceOnly(t *testing.T) {
	src := "type Money = { amount: number };\nexport interface Wallet { add(m: Money): void; }\n"
	units := splitTree("money.ts", []byte(src), tsLangs["ts"])
	if n := countKind(units, "Interface"); n != 1 {
		t.Errorf("want 1 Interface (interface_declaration only, not type_alias_declaration), got %d", n)
	}
}

func countKind(units []Unit, kind string) int {
	n := 0
	for _, u := range units {
		if u.Kind == kind {
			n++
		}
	}
	return n
}

func TestSplitGoCommentSeesWholeDeclaration(t *testing.T) {
	body := strings.Repeat("\tx++\n", 20)
	src := "package p\n\n// build ends by computing a digest.\nfunc build(x int) int {\n" + body + "\treturn digest(x)\n}\n\n// loose note\n"
	var doc, loose *Unit
	for _, u := range splitGo("p.go", []byte(src)) {
		switch {
		case u.Kind == "Comment" && u.Start == 3:
			doc = &u
		case u.Kind == "Comment":
			loose = &u
		}
	}
	if doc == nil || !strings.Contains(doc.Context, "return digest(x)") || !strings.Contains(doc.Context, "package p") {
		t.Fatalf("doc comment context must cover its function and nearby lines: %+v", doc)
	}
	if loose == nil || !strings.Contains(loose.Context, "return digest(x)") || strings.Contains(loose.Context, "func build") {
		t.Fatalf("top-level comment should keep the nearby-lines window: %+v", loose)
	}
}

func TestSplitGoFunctionSeesItsDocComment(t *testing.T) {
	src := "package p\n\n// put is best-effort: a failed write only costs a re-query.\nfunc put() {}\n\n// Store hides the backend.\ntype Store interface{ Get() }\n"
	got := map[string]string{}
	for _, u := range splitGo("p.go", []byte(src)) {
		got[u.Kind] = u.Context
	}
	if !strings.Contains(got["Function"], "best-effort") || !strings.Contains(got["Interface"], "hides the backend") {
		t.Fatalf("doc comments missing from unit context: %q", got)
	}
}
