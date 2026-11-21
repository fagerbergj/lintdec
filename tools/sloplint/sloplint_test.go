package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ladderSrc builds a function whose body is a run of `blocks` if-blocks.
func ladderSrc(name string, blocks int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "func %s(x int) int {\n\tvar y int\n", name)
	for i := 1; i <= blocks; i++ {
		fmt.Fprintf(&b, "\tif x > %d {\n\t\ty++\n\t}\n", i)
	}
	b.WriteString("\treturn y\n}\n")
	return b.String()
}

const aGo = `package p

// one
// two
// three
// four
` + "A_PLACEHOLDER" + `
// sloplint: cc-allow parity with the ts port's threshold, this one is a lookup ladder
` + "B_PLACEHOLDER" + `
func C(x int) int {
	if x > 0 {
		return 1
	}
	return 0
}
`

func writeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := strings.Replace(aGo, "A_PLACEHOLDER", ladderSrc("A", 17), 1)
	src = strings.Replace(src, "B_PLACEHOLDER", ladderSrc("B", 17), 1)
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// D repeats A's body: names erased, so any 80-token window matches.
	d := "package p\n\n" + ladderSrc("D", 20)
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte(d), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func any(findings []finding, substr string) bool {
	for _, f := range findings {
		if strings.Contains(f.message, substr) {
			return true
		}
	}
	return false
}

func TestComplexityFlagsOverThreshold(t *testing.T) {
	dir := writeTree(t)
	src, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	got := lintSource(filepath.Join(dir, "a.go"), src, nil)
	if !any(got, "4-line comment run") {
		t.Errorf("expected the 4-line comment run to be flagged, got: %v", got)
	}
	if !any(got, "A CC") {
		t.Errorf("expected A (CC 18) to be flagged, got: %v", got)
	}
	if any(got, "B CC") {
		t.Errorf("B has a cc-allow with a reason and must not be flagged: %v", got)
	}
	if any(got, "C CC") {
		t.Errorf("C (CC 2) must not be flagged: %v", got)
	}
}

func TestClonesMatchAcrossFiles(t *testing.T) {
	dir := writeTree(t)
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	srcA, _ := os.ReadFile(a)
	srcB, _ := os.ReadFile(b)
	tree := map[string][]byte{a: srcA, b: srcB}
	got := clones(tree, map[string][]span{a: nil, b: nil})
	if len(got) == 0 {
		t.Fatal("expected at least one clone between a.go and b.go")
	}
}

func TestChangedRanges(t *testing.T) {
	diff := `+++ b/a.go
@@ -1,4 +1,6 @@
 context
@@ -10 +11,3 @@
 more
`
	r := changedRanges(diff)
	if len(r["a.go"]) != 2 {
		t.Fatalf("want 2 ranges, got %v", r["a.go"])
	}
	if r["a.go"][0] != (span{1, 6}) {
		t.Errorf("first range: got %v want {1 6}", r["a.go"][0])
	}
	if r["a.go"][1] != (span{11, 13}) {
		t.Errorf("second range: got %v want {11 13}", r["a.go"][1])
	}
}
