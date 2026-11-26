package main

import (
	"strings"
	"testing"
)

func TestCandidateUnitsMarkOneWordPerQuestion(t *testing.T) {
	u := Unit{Kind: "Paragraph", File: "a.md", Start: 3, End: 4, Text: "The CDN served a `checksums.txt` that lied.\nIf a change is needed, PR it and let CI apply.\nEngineers know the rules. The agent knows the scope. UI should feel calm. Is Phase 3 still wanted? We'd still want a cap."}
	got := map[string]string{}
	for _, c := range candidateUnits(u, "DOCUMENT TITLE: x", "x") {
		got[c.Kind] += c.Text + "|"
	}
	if !strings.Contains(got["ActorVerb"], "⟦lied⟧") || strings.Contains(got["ActorVerb"], "⟦know⟧") || strings.Contains(got["ActorVerb"], "⟦knows⟧") || strings.Contains(got["ActorVerb"], "⟦feel⟧") || strings.Contains(got["ActorVerb"], "⟦wanted⟧") || strings.Contains(got["ActorVerb"], "⟦want⟧") {
		t.Errorf("ActorVerb candidates = %q", got["ActorVerb"])
	}
}

func TestMdSentencesDropsMarkupAndShortFragments(t *testing.T) {
	got := mdSentences("- See [the guide](x.md) for **details**. OK.\n<!-- note --> Run `make test` now!")
	want := []string{"See the guide for details.", "Run make test now!"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("mdSentences = %q, want %q", got, want)
	}
}

func TestMixedListOnlyWhenShapesDiffer(t *testing.T) {
	mixed := Unit{Kind: "Bullet", Text: "* Ability to operate without custom code\n* Ability to integrate regional data\n* The service will run in more than one region"}
	same := Unit{Kind: "Bullet", Text: "- **Owner**: the team that runs it\n- **Contact**: their Slack channel\n- **Cadence**: weekly"}
	one := Unit{Kind: "Bullet", Text: "- A single item that wraps\n  onto a second line"}
	if _, ok := mixedList(mixed); !ok {
		t.Error("noun-phrase items mixed with a sentence should be asked")
	}
	if _, ok := mixedList(same); ok {
		t.Error("label-then-detail items share a shape")
	}
	if _, ok := mixedList(one); ok {
		t.Error("a one-item list has nothing to compare")
	}
}

func TestMarkSpansFindsStockPhrases(t *testing.T) {
	got := markSpans("WordyPhrase", wordyPhrase, "In order to ship, the CLI is able to retry.", Unit{}, nil)
	if neg := markSpans("WordyPhrase", wordyPhrase, "It does not have the ability to retry and is unable to wait.", Unit{}, nil); len(neg) != 0 {
		t.Errorf("negations have no one-word swap: %+v", neg)
	}
	if len(got) != 2 || got[0].Text != "⟦In order to⟧ ship, the CLI is able to retry." || !strings.Contains(got[1].Text, "⟦is able to⟧") {
		t.Errorf("wordy spans = %+v", got)
	}
}
