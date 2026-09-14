package pr

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/remediate"
	"github.com/SaiPisey2/noisefloor/internal/scanner"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

var fixedWindowStart = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
var fixedWindowEnd = time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)

func locationFor(t *testing.T, alertName string) remediate.RuleLocation {
	t.Helper()
	locs, ferrs, err := remediate.LocateRules("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(ferrs) != 0 {
		t.Fatalf("unexpected file errors: %v", ferrs)
	}
	byKey := remediate.LocationsByKey(locs)
	loc, ok := byKey[remediate.RuleKey{Group: "fixture", AlertName: alertName}]
	if !ok {
		t.Fatalf("%s location not found in fixture", alertName)
	}
	return loc
}

func retireEval(t *testing.T) scanner.RuleEval {
	t.Helper()
	return scanner.RuleEval{
		Rule: store.Rule{
			GroupName: "fixture", AlertName: "RetireMe", Active: true,
		},
		Location: locationFor(t, "RetireMe"), HasLocation: true,
		HasEpisodes: true,
		WindowStart: fixedWindowStart, WindowEnd: fixedWindowEnd,
		Verdict: score.VerdictRetire, Noise: 45, Confidence: 0.85,
		Signals: score.Signals{Fires: 120, ShortLivedRate: 1.0, SilencedRate: 0.4},
		SilencedBy: []store.Silence{
			{
				AMID: "sil-1", CreatedBy: "alice", Comment: "known flaky, ticket OPS-123",
				StartsAt: fixedWindowStart.Add(48 * time.Hour), EndsAt: fixedWindowStart.Add(96 * time.Hour),
			},
		},
	}
}

func tuneEval(t *testing.T) scanner.RuleEval {
	t.Helper()
	durations := []time.Duration{
		180 * time.Second, 180 * time.Second, 180 * time.Second, 180 * time.Second,
		180 * time.Second, 180 * time.Second, 180 * time.Second,
		300 * time.Second, 300 * time.Second,
		900 * time.Second,
	}
	return scanner.RuleEval{
		Rule: store.Rule{
			GroupName: "fixture", AlertName: "TuneMe", Active: true, For: time.Minute,
		},
		Location: locationFor(t, "TuneMe"), HasLocation: true,
		HasEpisodes: true,
		WindowStart: fixedWindowStart, WindowEnd: fixedWindowEnd,
		Verdict: score.VerdictTune, Noise: 51, Confidence: 0.85,
		Signals:   score.Signals{Fires: 10, P90Duration: 300 * time.Second, FlapRate: 0.5},
		Durations: durations,
	}
}

func TestBuildRetireProposal(t *testing.T) {
	p, refusal, err := Build(retireEval(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if refusal != nil {
		t.Fatalf("unexpected refusal: %v", refusal)
	}
	if p == nil {
		t.Fatal("expected a proposal, got nil")
	}
	if p.Kind != KindRetire {
		t.Errorf("Kind = %q, want retire", p.Kind)
	}
	if p.Title != "noisefloor: retire RetireMe" {
		t.Errorf("Title = %q", p.Title)
	}
	if !strings.HasPrefix(p.Branch, "noisefloor/retire/fixture-retireme-") {
		t.Errorf("Branch = %q, want the deterministic retire branch shape", p.Branch)
	}
	if err := p.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(p.NewContent, "alert: RetireMe") {
		t.Error("rendered content still contains the deleted rule")
	}
	assertGolden(t, "testdata/golden/retire_body.golden.md", p.Body)
}

func TestBuildTuneProposal(t *testing.T) {
	p, refusal, err := Build(tuneEval(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if refusal != nil {
		t.Fatalf("unexpected refusal: %v", refusal)
	}
	if p == nil {
		t.Fatal("expected a proposal, got nil")
	}
	if p.Kind != KindTune {
		t.Errorf("Kind = %q, want tune", p.Kind)
	}
	if !strings.HasPrefix(p.Branch, "noisefloor/tune/fixture-tuneme-") {
		t.Errorf("Branch = %q, want the deterministic tune branch shape", p.Branch)
	}
	if err := p.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(p.NewContent, "for:    6m   # keep this pager quiet during a deploy") {
		t.Errorf("rendered content missing the expected replaced for: line:\n%s", p.NewContent)
	}
	assertGolden(t, "testdata/golden/tune_body.golden.md", p.Body)
}

// TestBuildInsertsForFieldWhenAbsent covers the tune shape with no
// existing for: line to replace (NoForField, verdict forced to tune here
// since the fixture's real signals would never earn one on their own).
func TestBuildInsertsForFieldWhenAbsent(t *testing.T) {
	eval := scanner.RuleEval{
		Rule:     store.Rule{GroupName: "fixture", AlertName: "NoForField", Active: true, For: 0},
		Location: locationFor(t, "NoForField"), HasLocation: true,
		HasEpisodes: true,
		WindowStart: fixedWindowStart, WindowEnd: fixedWindowEnd,
		Verdict: score.VerdictTune, Confidence: 0.9,
		Signals:   score.Signals{Fires: 12, P90Duration: 2 * time.Minute},
		Durations: []time.Duration{time.Minute, time.Minute, 2 * time.Minute},
	}
	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if refusal != nil {
		t.Fatalf("unexpected refusal: %v", refusal)
	}
	if err := p.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(p.NewContent, "        expr: up == 0\n        for: 2m\n") {
		t.Errorf("expected a new for: line inserted after expr:, got:\n%s", p.NewContent)
	}
}

func TestBuildRefusesAmbiguous(t *testing.T) {
	eval := retireEval(t)
	eval.Ambiguous = true
	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal for an ambiguous rule")
	}
	if refusal == nil || refusal.Reason != ReasonAmbiguous {
		t.Errorf("refusal = %+v, want ReasonAmbiguous", refusal)
	}
}

func TestBuildRefusesInactive(t *testing.T) {
	eval := retireEval(t)
	eval.Rule.Active = false
	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal for an inactive rule")
	}
	if refusal == nil || refusal.Reason != ReasonInactive {
		t.Errorf("refusal = %+v, want ReasonInactive", refusal)
	}
}

func TestBuildRefusesRetuned(t *testing.T) {
	eval := retireEval(t)
	eval.Retuned = true
	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal for a retuned rule")
	}
	if refusal == nil || refusal.Reason != ReasonRetuned {
		t.Errorf("refusal = %+v, want ReasonRetuned", refusal)
	}
}

func TestBuildRefusesLowConfidence(t *testing.T) {
	eval := retireEval(t)
	eval.Confidence = 0.1
	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal below the confidence floor")
	}
	if refusal == nil || refusal.Reason != ReasonLowConfidence {
		t.Errorf("refusal = %+v, want ReasonLowConfidence", refusal)
	}
}

func TestBuildRefusesNoLocation(t *testing.T) {
	eval := retireEval(t)
	eval.HasLocation = false
	eval.Location = remediate.RuleLocation{}
	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal with no known location")
	}
	if refusal == nil || refusal.Reason != ReasonNoLocation {
		t.Errorf("refusal = %+v, want ReasonNoLocation", refusal)
	}
}

func TestBuildIsNilNilForAKeepVerdict(t *testing.T) {
	eval := retireEval(t)
	eval.Verdict = score.VerdictKeep
	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil || refusal != nil {
		t.Errorf("expected (nil, nil) for a keep verdict, got (%v, %v)", p, refusal)
	}
}

func TestBuildIsNilNilForAnAutomateVerdict(t *testing.T) {
	eval := retireEval(t)
	eval.Verdict = score.VerdictAutomate
	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil || refusal != nil {
		t.Errorf("expected (nil, nil) for an automate verdict, got (%v, %v)", p, refusal)
	}
}

func assertGolden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata/golden", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file %s: %v (run with UPDATE_GOLDEN=1 to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("output does not match golden file %s.\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}
