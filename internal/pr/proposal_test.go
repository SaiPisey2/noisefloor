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
			// Expr and For are what PROMETHEUS reports for this rule. They
			// have to agree with the fixture on disk or Build refuses the
			// proposal as drifted, which is the point of that check.
			Expr: "up == 0",
		},
		Location: locationFor(t, "RetireMe"), HasLocation: true,
		HasEpisodes: true,
		WindowStart: fixedWindowStart, WindowEnd: fixedWindowEnd,
		Verdict: score.VerdictRetire, Noise: 45, Confidence: 0.85,
		Signals:           score.Signals{Fires: 120, ShortLivedRate: 1.0, SilencedRate: 0.4},
		SilencesAvailable: true,
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
			Expr: "rate(errors_total[5m]) > 0",
		},
		Location: locationFor(t, "TuneMe"), HasLocation: true,
		HasEpisodes: true,
		WindowStart: fixedWindowStart, WindowEnd: fixedWindowEnd,
		Verdict: score.VerdictTune, Noise: 51, Confidence: 0.85,
		Signals:           score.Signals{Fires: 10, P90Duration: 300 * time.Second, FlapRate: 0.5},
		SilencesAvailable: true,
		Durations:         durations,
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
		Rule: store.Rule{
			GroupName: "fixture", AlertName: "NoForField", Active: true, For: 0,
			Expr: "up == 0",
		},
		Location: locationFor(t, "NoForField"), HasLocation: true,
		HasEpisodes: true,
		WindowStart: fixedWindowStart, WindowEnd: fixedWindowEnd,
		Verdict: score.VerdictTune, Confidence: 0.9,
		Signals:           score.Signals{Fires: 12, P90Duration: 2 * time.Minute},
		SilencesAvailable: true,
		Durations:         []time.Duration{time.Minute, time.Minute, 2 * time.Minute},
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

// TestBuildRefusesStaleForValue is the stale-checkout defect. Every number
// in the body -- including the `for:` the body says it is raising -- comes
// from Prometheus; the diff is computed against the file. When the checkout
// is behind, the body reads "Raise `for: 1m` to `for: 6m`" directly above a
// diff that reads `- for: 45s`, and nothing in the PR discloses it.
func TestBuildRefusesStaleForValue(t *testing.T) {
	eval := tuneEval(t)
	eval.Rule.For = 45 * time.Second // the file says 1m; prometheus says 45s

	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal against a checkout that disagrees with prometheus")
	}
	if refusal == nil || refusal.Reason != ReasonDrift {
		t.Fatalf("refusal = %+v, want ReasonDrift", refusal)
	}
	// Both values, so the operator can tell which side is stale without
	// opening either.
	for _, want := range []string{"1m", "45s"} {
		if !strings.Contains(refusal.Detail, want) {
			t.Errorf("refusal detail %q does not name %q", refusal.Detail, want)
		}
	}
}

// TestBuildRefusesStaleExpr is the same defect on the expression: the file
// defines a different rule from the one whose history was scored, so the
// evidence in the body is about something else entirely.
func TestBuildRefusesStaleExpr(t *testing.T) {
	eval := retireEval(t)
	eval.Rule.Expr = "up == 0 and on() cluster_healthy == 1"

	_, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if refusal == nil || refusal.Reason != ReasonDrift {
		t.Fatalf("refusal = %+v, want ReasonDrift", refusal)
	}
	if !strings.Contains(refusal.Detail, "up == 0") ||
		!strings.Contains(refusal.Detail, "cluster_healthy") {
		t.Errorf("refusal detail %q does not name both expressions", refusal.Detail)
	}
}

// TestBuildAcceptsEquivalentForSpelling keeps the drift check from refusing
// on cosmetics: `60s` and `1m` are the same rule written two ways, and
// Prometheus reports durations in its own normalised form.
func TestBuildAcceptsEquivalentForSpelling(t *testing.T) {
	eval := tuneEval(t)
	eval.Location.ForValue = "60s" // the file's spelling; eval.Rule.For is 1m

	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if refusal != nil {
		t.Fatalf("unexpected refusal for an equivalently-spelled for:: %v", refusal)
	}
	if p == nil {
		t.Fatal("expected a proposal")
	}
}

// TestBuildAcceptsReflowedBlockScalarExpr is the same guard for `expr:`.
// Prometheus re-serialises an expression from its parsed form, so an
// `expr: |` block scalar comes back as a single line. Comparing raw text
// would report drift on every multi-line expression in the repository.
func TestBuildAcceptsReflowedBlockScalarExpr(t *testing.T) {
	eval := retireEval(t)
	eval.Location.ExprValue = "up\n  ==\n  0\n"
	eval.Rule.Expr = "up == 0"

	_, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if refusal != nil {
		t.Fatalf("unexpected refusal for a reflowed block scalar: %v", refusal)
	}
}

// TestBuildRefusesDuplicateLocation covers two rules with the same alert
// name in one group: legal Prometheus, invisible to the cross-group
// ambiguity check, and a coin toss over which one gets edited.
func TestBuildRefusesDuplicateLocation(t *testing.T) {
	eval := retireEval(t)
	eval.Location.Duplicate = true

	p, refusal, err := Build(eval)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal for a duplicated (group, alertname)")
	}
	if refusal == nil || refusal.Reason != ReasonDuplicate {
		t.Fatalf("refusal = %+v, want ReasonDuplicate", refusal)
	}
	if !strings.Contains(refusal.Detail, "fixture.yml") {
		t.Errorf("refusal detail %q does not point at the file", refusal.Detail)
	}
}
