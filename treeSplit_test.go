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
	foundFn := false
	for _, u := range units {
		if u.Kind == "Function" {
			foundFn = true
			if strings.Contains(u.Context, "unrelated") {
				t.Errorf("blank-line-separated comment leaked into doc context: %q", u.Context)
			}
			if !strings.Contains(u.Context, "doc for greet") {
				t.Errorf("adjacent doc comment missing from context: %q", u.Context)
			}
		}
	}
	if !foundFn {
		t.Fatal("expected Function unit for greet")
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
	if doc == nil || !strings.Contains(doc.Context, "return digest(x)") || strings.Contains(doc.Context, "package p") {
		t.Fatalf("doc comment context must cover only its declaration, not unrelated neighbors: %+v", doc)
	}
	if loose == nil || !strings.Contains(loose.Context, "return digest(x)") || strings.Contains(loose.Context, "func build") {
		t.Fatalf("top-level comment should keep the nearby-lines window: %+v", loose)
	}
}

func TestAttachTestHelpersIncludesCalledHelpersOnly(t *testing.T) {
	tsSrc := `async function within(ms: number, done: () => boolean) {
  for (let t = 0; t < ms && !done(); t += 500) await clock.advance(500);
  return done();
}
function unusedHelper() { return 1; }
test('waits using fake clock', async () => {
  assert.ok(await within(5000, () => true));
});
test('does not call helpers', () => {
  obj.within(10);
});
`
	units := splitFile("cap.test.ts", []byte(tsSrc))
	var tests []Unit
	for _, u := range units {
		if u.Kind == "TestFunction" {
			tests = append(tests, u)
		}
	}
	if len(tests) != 2 {
		t.Fatalf("want 2 TestFunction units, got %d", len(tests))
	}
	if !strings.Contains(tests[0].Context, "clock.advance(500)") || strings.Contains(tests[0].Context, "unusedHelper") {
		t.Errorf("first test should see called helper within only:\n%s", tests[0].Context)
	}
	if strings.Contains(tests[1].Context, "clock.advance(500)") {
		t.Errorf("method call obj.within should not attach top-level helper:\n%s", tests[1].Context)
	}

	// Go test file: top-level helper attached, receiver method skipped.
	goSrc := `package p
type clock struct{}
func (c *clock) advance() {}
func fakeWait() { println("fake") }
func TestRun(t *testing.T) { fakeWait() }
`
	for _, u := range splitFile("run_test.go", []byte(goSrc)) {
		if u.Kind == "TestFunction" {
			if !strings.Contains(u.Context, "4\tfunc fakeWait()") || strings.Contains(u.Context, "advance") {
				t.Errorf("Go test helper context wrong:\n%s", u.Context)
			}
		}
	}

	// Class methods are excluded from the top-level helper pool.
	clsSrc := `class Draft { render() { return 'class-method'; } }
function render() { return 'top-level'; }
test('renders', () => { expect(render()).toBe('top-level'); });
`
	for _, u := range splitFile("cls.test.ts", []byte(clsSrc)) {
		if u.Kind == "TestFunction" {
			if !strings.Contains(u.Context, "top-level") || strings.Contains(u.Context, "class-method") {
				t.Errorf("class method should not attach as top-level helper:\n%s", u.Context)
			}
		}
	}

	// Budget caps: >30-line helper skipped; total helper context capped at 50 lines.
	body := strings.Repeat("  const x = 1;\n", 26)
	capSrc := "function hOne() {\n" + body + "}\nfunction hTwo() {\n" + body + "}\nfunction huge() {\n" + strings.Repeat("  const y = 2;\n", 31) + "}\ntest('caps', () => { hOne(); hTwo(); huge(); });\n"
	for _, u := range splitFile("caps.test.ts", []byte(capSrc)) {
		if u.Kind == "TestFunction" {
			if !strings.Contains(u.Context, "function hOne") || strings.Contains(u.Context, "function hTwo") || strings.Contains(u.Context, "function huge") {
				t.Errorf("helper caps not enforced:\n%s", u.Context)
			}
		}
	}
}

func TestTrailingCommentsKeepNearbyContext(t *testing.T) {
	tsSrc := `const ROUTES = [
  ['GET', '/a'],
]; // one entry per route
export function demo() { return 1; }
`
	for _, u := range splitTree("routes.ts", []byte(tsSrc), tsLangs["ts"]) {
		if u.Kind == "Comment" {
			if !strings.Contains(u.Context, "const ROUTES") {
				t.Errorf("trailing TS comment lost the declaration it trails:\n%s", u.Context)
			}
		}
	}

	goSrc := "package p\nconst x = 1\nfunc f() {} // short trailing note\n"
	for _, u := range splitGo("trail.go", []byte(goSrc)) {
		if u.Kind == "Comment" && !strings.Contains(u.Context, "const x = 1") {
			t.Errorf("single-line Go trailing comment lost nearby context:\n%s", u.Context)
		}
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

func TestSplitTreeTSDescribeAndCallbacks(t *testing.T) {
	src := `before(async () => {
  const localHelper = () => 1;
});
beforeEach(() => {});
const ROUTES = [['GET', () => 200]];
const TABLE = { add: () => 1, rm: () => 2 };
const mapped = items.map((x) => x + 1);
const outer = () => {
  const inner = () => 2;
  return inner();
};
describe('suite', () => {
  it('first test', () => { expect(1).toBe(1); });
  it.skip('second test', () => { expect(2).toBe(2); });
  test('third test', () => { expect(3).toBe(3); });
  it.todo('not implemented yet');
});
`
	units := splitTree("suite.test.ts", []byte(src), tsLangs["ts"])
	if n := countKind(units, "TestFunction"); n != 3 {
		t.Errorf("want 3 TestFunction units (it, it.skip, test; not describe/before/todo), got %d: %+v", n, units)
	}
	if n := countKind(units, "Function"); n != 1 {
		t.Errorf("want 1 Function unit (outer only; not ROUTES/TABLE/mapped/localHelper/inner), got %d: %+v", n, units)
	}
}

func TestSplitTreeTSDocAndInlineComments(t *testing.T) {
	src := `#!/usr/bin/env node
// line 1 of doc
// line 2 of doc
export const run = () => {
  //lintdec:ignore comment-tautology
  // inline note
  return 1;
};
`
	units := splitTree("run.ts", []byte(src), tsLangs["ts"])
	var comments []Unit
	for _, u := range units {
		if u.Kind == "Comment" && !structuralComment(u) {
			comments = append(comments, u)
		}
	}
	if len(comments) != 2 {
		t.Fatalf("want 2 non-structural Comment units (grouped 2-line doc + inline note, skipping shebang & ignore), got %d: %+v", len(comments), comments)
	}
	if comments[0].Start != 2 || comments[0].End != 3 {
		t.Errorf("doc comment split wrong: lines %d-%d, want 2-3", comments[0].Start, comments[0].End)
	}
	if !strings.Contains(comments[1].Text, "inline note") || comments[1].Start != 6 {
		t.Errorf("missing or merged inline comment: %+v", comments[1])
	}
}

func TestIsFnExprWrappers(t *testing.T) {
	src := `const a = ((x: number) => x + 1);
const b = ((x: number) => x + 1) as (x: number) => number;
const c = ((x: number) => x + 1) satisfies (x: number) => number;
const d = function (x: number) { return x + 1; };
const e = (123) as number;
`
	units := splitTree("wrappers.ts", []byte(src), tsLangs["ts"])
	if n := countKind(units, "Function"); n != 4 {
		t.Errorf("want 4 Function units (paren, as, satisfies, function_expression; not number), got %d: %+v", n, units)
	}
}

func TestTreeCommentContextInsideFunction(t *testing.T) {
	var b strings.Builder
	b.WriteString("export function process(firstLineToken: string) {\n")
	for range 12 {
		b.WriteString("  const step = 1;\n")
	}
	b.WriteString("  // references firstLineToken and lastLineToken\n")
	for range 12 {
		b.WriteString("  const more = 2;\n")
	}
	b.WriteString("  return 'lastLineToken';\n}\n")
	units := splitTree("inside.ts", []byte(b.String()), tsLangs["ts"])
	for _, u := range units {
		if u.Kind == "Comment" {
			if !strings.Contains(u.Context, "firstLineToken") || !strings.Contains(u.Context, "lastLineToken") {
				t.Errorf("in-function comment context missed enclosing declaration:\n%s", u.Context)
			}
			return
		}
	}
	t.Fatal("missing Comment unit")
}

func TestTreeCommentContextCoversFollowingFunction(t *testing.T) {
	var b strings.Builder
	b.WriteString("const unrelatedBefore = 1;\n\n// mentions lineSixteenToken\nexport function demo() {\n")
	for range 12 {
		b.WriteString("  const x = 1;\n")
	}
	b.WriteString("  return 'lineSixteenToken';\n}\n\nconst unrelatedAfter = 2;\n")
	units := splitTree("demo.ts", []byte(b.String()), tsLangs["ts"])
	for _, u := range units {
		if u.Kind == "Comment" {
			if !strings.Contains(u.Context, "lineSixteenToken") {
				t.Errorf("doc comment context truncated before end of function:\n%s", u.Context)
			}
			if strings.Contains(u.Context, "unrelatedBefore") || strings.Contains(u.Context, "unrelatedAfter") {
				t.Errorf("doc comment context should not include unrelated neighbors:\n%s", u.Context)
			}
			return
		}
	}
	t.Fatal("missing Comment unit")
}
