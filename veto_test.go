package main

import (
	"slices"
	"testing"
)

func TestReportableAppliesFloorAndVeto(t *testing.T) {
	flakey, _ := findRule("flakey-test")
	low, high := 0.1, 0.9
	cases := []struct {
		name      string
		v         Verdict
		calibrate bool
		want      bool
	}{
		{"below floor", Verdict{Lint: true, Confidence: 0.79}, false, false},
		{"above floor, no veto", Verdict{Lint: true, Confidence: 0.85}, false, true},
		{"vetoed", Verdict{Lint: true, Confidence: 0.95, Veto: &high}, false, false},
		{"veto below cut", Verdict{Lint: true, Confidence: 0.95, Veto: &low}, false, true},
		{"calibrate ignores floor and veto", Verdict{Lint: true, Confidence: 0.6, Veto: &high}, true, true},
		{"not lint", Verdict{Confidence: 0.99}, true, false},
	}
	for _, c := range cases {
		if got := reportable(flakey, c.v, c.calibrate); got != c.want {
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
