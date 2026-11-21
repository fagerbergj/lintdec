package main

import (
	"context"
	"strings"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
)

const probeSrc = `import { readdirSync } from 'node:fs';
/** doc for arrowProblem */
export function arrowProblem(from: string, to: string): string | undefined {
  if (from === to) return undefined;
  return 'x';
}
type Edge = { from: string; to: string };
export const process = (a: string) => a.length;
`

// TestProbeText checks that Content() returns source text, not S-expressions.
func TestProbeText(t *testing.T) {
	src := []byte(probeSrc)
	root, err := sitter.ParseCtx(context.Background(), src, tsx.GetLanguage())
	if err != nil {
		t.Fatal(err)
	}
	for i, count := 0, int(root.ChildCount()); i < count; i++ {
		c := root.Child(i)
		t.Logf("%d: type=%s named=%v name=%q text=%.50q", i, c.Type(), c.IsNamed(), declName(c, src), c.Content(src))
	}
}

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

func countKind(units []Unit, kind string) int {
	n := 0
	for _, u := range units {
		if u.Kind == kind {
			n++
		}
	}
	return n
}
