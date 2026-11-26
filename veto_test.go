package main

import (
	"slices"
	"strings"
	"testing"
)

func TestReportableAppliesFloorAndVeto(t *testing.T) {
	flakey, _ := findRule("flakey-test")
	low, high := 0.1, 0.9
	cases := []struct {
		name      string
		v         Verdict
		floor     float64
		calibrate bool
		want      bool
	}{
		{"below floor", Verdict{Lint: true, Confidence: 0.79}, ruleFloor("flakey-test"), false, false},
		{"above floor, no veto", Verdict{Lint: true, Confidence: 0.85}, ruleFloor("flakey-test"), false, true},
		{"vetoed", Verdict{Lint: true, Confidence: 0.95, Veto: &high}, ruleFloor("flakey-test"), false, false},
		{"veto below cut", Verdict{Lint: true, Confidence: 0.95, Veto: &low}, ruleFloor("flakey-test"), false, true},
		{"override lowers floor, keeps veto pass", Verdict{Lint: true, Confidence: 0.70, Veto: &low}, 0.65, false, true},
		{"override lowers floor, keeps veto block", Verdict{Lint: true, Confidence: 0.70, Veto: &high}, 0.65, false, false},
		{"override raises floor", Verdict{Lint: true, Confidence: 0.85}, 0.90, false, false},
		{"calibrate ignores floor and veto", Verdict{Lint: true, Confidence: 0.6, Veto: &high}, ruleFloor("flakey-test"), true, true},
		{"not lint", Verdict{Confidence: 0.99}, ruleFloor("flakey-test"), true, false},
	}
	for _, c := range cases {
		if got := reportable(flakey, c.v, c.floor, c.calibrate); got != c.want {
			t.Errorf("%s: reportable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWithVetoesAddsExceptionQuestionsWithoutMutatingInput(t *testing.T) {
	flakey, _ := findRule("flakey-test")
	todo, _ := findRule("todo-no-owner")
	in := make([]Rule, 2, 4)
	in[0], in[1] = flakey, todo
	got := withVetoes(in)
	if len(got) != 3 || got[2].ID != "flakey-test~veto" || got[2].Scope != "question" || len(in) != 2 || cap(in) == 0 {
		t.Fatalf("withVetoes = %+v", got)
	}
	if v := attachVetoes(in, map[string]Verdict{"flakey-test": {Lint: true}, "flakey-test~veto": {Confidence: 0.4}, "todo-no-owner": {}}); len(v) != 2 || v["flakey-test"].Veto == nil || *v["flakey-test"].Veto != 0.4 || v["todo-no-owner"].Veto != nil {
		t.Fatalf("attachVetoes = %+v", v)
	}
}

func TestDefaultOffRulesNeedAnExplicitRequest(t *testing.T) {
	active, err := resolveRules("", ProjectConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range active {
		if defaultOff[r.ID] {
			t.Fatalf("%s is default-off but active", r.ID)
		}
	}
	named, _ := resolveRules("stub-body", ProjectConfig{})
	on, _ := resolveRules("", ProjectConfig{RulesOn: []string{"stub-body"}, RulesOff: []string{"md-prose-list"}})
	if len(named) != 1 || len(on) != len(active) || !slices.ContainsFunc(on, func(r Rule) bool { return r.ID == "stub-body" }) ||
		slices.ContainsFunc(on, func(r Rule) bool { return r.ID == "md-prose-list" }) {
		t.Fatalf("on: must add to and off: remove from the defaults: %v %v", ruleIDList(named), ruleIDList(on))
	}
	if got, err := resolveRules("", ProjectConfig{RulesOn: []string{"no-such-rule"}}); err != nil || len(got) != len(active) {
		t.Fatalf("an unknown lintdec.yaml rule must be ignored, not fail the run: %v", err)
	}
}

// Offline shape check, like the init() invariants in rules.go: a citation a
// reader cannot follow is not a citation. No fetches here, so the suite stays fast.
func TestEveryRuleHasSource(t *testing.T) {
	for _, r := range rules {
		if !strings.Contains(r.Source, "://") {
			t.Errorf("rule %s must cite a primary source with a URL, got %q", r.ID, r.Source)
		}
	}
}
