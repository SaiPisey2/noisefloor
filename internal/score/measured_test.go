package score

import (
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/enrich"
)

func outcome(fp string, acked, escalated bool) enrich.EpisodeOutcome {
	return enrich.EpisodeOutcome{Fingerprint: fp, Acknowledged: acked, Escalated: escalated}
}

func TestAggregateOutcomesNilWhenNothingMatched(t *testing.T) {
	if got := AggregateOutcomes(enrich.Result{Source: "pagerduty"}, 10); got != nil {
		t.Errorf("got %+v, want nil for an empty Matched", got)
	}
	if got := AggregateOutcomes(enrich.Result{Matched: []enrich.EpisodeOutcome{outcome("a", true, false)}}, 10); got != nil {
		t.Errorf("got %+v, want nil for an empty Source", got)
	}
	if got := AggregateOutcomes(enrich.Result{Source: "pagerduty", Matched: []enrich.EpisodeOutcome{outcome("a", true, false)}}, 0); got != nil {
		t.Errorf("got %+v, want nil for zero firing episodes", got)
	}
}

func TestAggregateOutcomesComputesRates(t *testing.T) {
	res := enrich.Result{
		Source: "pagerduty",
		Matched: []enrich.EpisodeOutcome{
			outcome("a", true, false),
			outcome("b", true, true),
			outcome("c", false, false),
			outcome("d", false, false),
		},
	}
	got := AggregateOutcomes(res, 8) // 4 matched / 8 firing = 50% coverage
	if got == nil {
		t.Fatal("got nil, want a result")
	}
	if got.Matched != 4 {
		t.Errorf("Matched = %d, want 4", got.Matched)
	}
	if got.Coverage != 0.5 {
		t.Errorf("Coverage = %v, want 0.5", got.Coverage)
	}
	if got.AckRate != 0.5 {
		t.Errorf("AckRate = %v, want 0.5", got.AckRate)
	}
	if got.EscalationRate != 0.25 {
		t.Errorf("EscalationRate = %v, want 0.25", got.EscalationRate)
	}
}

func TestAggregateOutcomesClampsImpossibleCoverage(t *testing.T) {
	res := enrich.Result{Source: "pagerduty", Matched: []enrich.EpisodeOutcome{
		outcome("a", true, false), outcome("b", true, false), outcome("c", true, false),
	}}
	got := AggregateOutcomes(res, 1)
	if got.Coverage != 1 {
		t.Errorf("Coverage = %v, want clamped to 1", got.Coverage)
	}
}

// --- Confidence -------------------------------------------------------

func TestMeasuredCoverageRaisesConfidenceModestly(t *testing.T) {
	// Fires well below min_episodes (10) so the base confidence sits below
	// 1 and the measured boost has room to move it -- a fixture already at
	// the 1.0 ceiling can't demonstrate a bounded multiplicative boost.
	base := Signals{Fires: 5, UniqueFingerprints: 5}
	c := defaultConfidence()
	without := Confidence(base, 30*24*time.Hour, c)

	measured := base
	measured.PagerOutcomes = &PagerOutcomes{Source: "pagerduty", Coverage: 1, Matched: 100, AckRate: 0.5}
	with := Confidence(measured, 30*24*time.Hour, c)

	if with <= without {
		t.Errorf("full measured coverage did not raise confidence: without=%v with=%v", without, with)
	}
	if with > 1 {
		t.Errorf("confidence must stay clamped to 1, got %v", with)
	}
}

// --- Verdict: valuable rule guard --------------------------------------

// TestHighEngagementDowngradesRetireToTune pins the asymmetric case the
// design explicitly calls for: pages that were consistently acknowledged
// and escalated are evidence the rule is VALUABLE, which must push away
// from retire regardless of how noisy the duration-derived signals look.
func TestHighEngagementDowngradesRetireToTune(t *testing.T) {
	s := confident(Signals{ShortLivedRate: 1, SilencedRate: 1}) // would retire on its own
	c := defaultConfidence()
	noise := NoiseScore(s, defaultWeights())
	conf := Confidence(s, 30*24*time.Hour, c)
	if got := Verdict(s, noise, conf, c); got != VerdictRetire {
		t.Fatalf("precondition: base verdict = %s, want retire", got)
	}

	s.PagerOutcomes = &PagerOutcomes{
		Source: "pagerduty", Coverage: 0.9, Matched: 90, AckRate: 0.8, EscalationRate: 0.1,
	}
	conf = Confidence(s, 30*24*time.Hour, c)
	if got := Verdict(s, noise, conf, c); got != VerdictTune {
		t.Errorf("verdict with high measured engagement = %s, want tune (downgraded from retire)", got)
	}
}

// TestLowCoverageEngagementDoesNotGuardRetire pins the coverage floor: a
// handful of acknowledged pages out of a much larger, mostly-unmatched
// rule must not be trusted to say the rule is valuable.
func TestLowCoverageEngagementDoesNotGuardRetire(t *testing.T) {
	s := confident(Signals{ShortLivedRate: 1, SilencedRate: 1})
	c := defaultConfidence()
	s.PagerOutcomes = &PagerOutcomes{
		Source: "pagerduty", Coverage: 0.05, Matched: 5, AckRate: 1, EscalationRate: 1,
	}
	noise := NoiseScore(s, defaultWeights())
	conf := Confidence(s, 30*24*time.Hour, c)
	if got := Verdict(s, noise, conf, c); got != VerdictRetire {
		t.Errorf("verdict = %s, want retire unchanged (coverage too low to trust)", got)
	}
}

// --- Verdict: never-acknowledged strengthens retire ---------------------

// TestNeverAcknowledgedUpgradesKeepToRetire pins the other named
// asymmetric case: a rule that paged repeatedly and was essentially never
// acknowledged is a stronger retire case than duration-based signals
// alone, even when those signals land on keep.
func TestNeverAcknowledgedUpgradesKeepToRetire(t *testing.T) {
	s := confident(Signals{}) // clean on every duration-derived signal -> keep
	c := defaultConfidence()
	noise := NoiseScore(s, defaultWeights())
	conf := Confidence(s, 30*24*time.Hour, c)
	if got := Verdict(s, noise, conf, c); got != VerdictKeep {
		t.Fatalf("precondition: base verdict = %s, want keep", got)
	}

	s.PagerOutcomes = &PagerOutcomes{
		Source: "pagerduty", Coverage: 1, Matched: 400, AckRate: 0, EscalationRate: 0,
	}
	conf = Confidence(s, 30*24*time.Hour, c)
	if got := Verdict(s, noise, conf, c); got != VerdictRetire {
		t.Errorf("verdict with 400 never-acked measured pages = %s, want retire", got)
	}
}

// TestHumanResolvedBlocksNeverAckedUpgrade is finding 2: engagementRate is
// max(AckRate, EscalationRate) and never looks at HumanResolvedRate, so a
// team that resolves from the push notification without formally
// acknowledging the page reads as "never engaged" and gets its rule
// upgraded to retire on eighty pages a human personally closed. The
// upgrade must not fire while HumanResolvedRate contradicts it.
func TestHumanResolvedBlocksNeverAckedUpgrade(t *testing.T) {
	s := confident(Signals{}) // clean on every duration-derived signal -> keep
	c := defaultConfidence()
	noise := NoiseScore(s, defaultWeights())
	conf := Confidence(s, 30*24*time.Hour, c)
	if got := Verdict(s, noise, conf, c); got != VerdictKeep {
		t.Fatalf("precondition: base verdict = %s, want keep", got)
	}

	s.PagerOutcomes = &PagerOutcomes{
		Source: "pagerduty", Coverage: 1, Matched: 80,
		AckRate: 0, EscalationRate: 0, HumanResolvedRate: 1,
	}
	conf = Confidence(s, 30*24*time.Hour, c)
	if got := Verdict(s, noise, conf, c); got != VerdictKeep {
		t.Errorf("verdict with HumanResolvedRate 1.0 = %s, want keep unchanged "+
			"(a human personally closed every one of these pages)", got)
	}
}

// TestNeverAcknowledgedRequiresLargeSample guards the higher evidentiary
// bar this arm is held to: it makes a stronger claim than the coverage
// floor alone, so it needs neverAckedMinMatched, not just
// measuredMinMatched.
func TestNeverAcknowledgedRequiresLargeSample(t *testing.T) {
	s := confident(Signals{})
	c := defaultConfidence()
	s.PagerOutcomes = &PagerOutcomes{
		Source: "pagerduty", Coverage: 1, Matched: neverAckedMinMatched - 1, AckRate: 0,
	}
	noise := NoiseScore(s, defaultWeights())
	conf := Confidence(s, 30*24*time.Hour, c)
	if got := Verdict(s, noise, conf, c); got != VerdictKeep {
		t.Errorf("verdict = %s, want keep unchanged (sample below neverAckedMinMatched)", got)
	}
}

// TestNeverAcknowledgedNeverTouchesTuneOrAutomate pins the deliberate
// narrowness: this arm only ever upgrades keep, never a rule already
// flagged as having a different, already-identified problem.
func TestNeverAcknowledgedNeverTouchesTuneOrAutomate(t *testing.T) {
	c := defaultConfidence()
	never := &PagerOutcomes{Source: "pagerduty", Coverage: 1, Matched: 400, AckRate: 0}

	flapping := confident(Signals{FlapRate: 1})
	flapping.PagerOutcomes = never
	noise := NoiseScore(flapping, defaultWeights())
	conf := Confidence(flapping, 30*24*time.Hour, c)
	if got := Verdict(flapping, noise, conf, c); got != VerdictTune {
		t.Errorf("flapping rule verdict = %s, want tune unchanged", got)
	}

	automate := confident(Signals{
		Fires: automateMinFires, P50Duration: automateMinDuration, ShortLivedRate: 0,
	})
	automate.PagerOutcomes = never
	noise = NoiseScore(automate, defaultWeights())
	conf = Confidence(automate, 30*24*time.Hour, c)
	if got := Verdict(automate, noise, conf, c); got != VerdictAutomate {
		t.Errorf("automate-candidate rule verdict = %s, want automate unchanged", got)
	}
}

// TestMeasuredEvidenceNeverOverridesTheConfidenceFloor guards the ordering
// invariant: a rule below min_episodes or the confidence floor must stay
// keep no matter what a (hypothetically attached) PagerOutcomes says --
// measured evidence augments an already-qualifying verdict, it does not
// bypass the floor that exists so noisefloor never proposes on thin
// evidence.
func TestMeasuredEvidenceNeverOverridesTheConfidenceFloor(t *testing.T) {
	c := defaultConfidence()
	s := Signals{Fires: 2} // below min_episodes (10)
	s.PagerOutcomes = &PagerOutcomes{Source: "pagerduty", Coverage: 1, Matched: 400, AckRate: 0}
	if got := Verdict(s, 0, 1, c); got != VerdictKeep {
		t.Errorf("verdict = %s, want keep (min_episodes floor must win)", got)
	}
}

// TestNilPagerOutcomesChangesNothing is the demo-safety guard: absent an
// enricher, every rule's Signals.PagerOutcomes is nil, and every scoring
// path above must produce exactly what it always did.
func TestNilPagerOutcomesChangesNothing(t *testing.T) {
	c := defaultConfidence()
	for _, s := range []Signals{
		confident(Signals{ShortLivedRate: 1, SilencedRate: 1}),
		confident(Signals{}),
		confident(Signals{FlapRate: 1}),
	} {
		noise := NoiseScore(s, defaultWeights())
		wantConf := Confidence(s, 30*24*time.Hour, c)
		wantVerdict := Verdict(s, noise, wantConf, c)

		s.PagerOutcomes = nil // explicit, though it already is
		if got := Confidence(s, 30*24*time.Hour, c); got != wantConf {
			t.Errorf("confidence changed with nil PagerOutcomes: got %v want %v", got, wantConf)
		}
		if got := Verdict(s, noise, wantConf, c); got != wantVerdict {
			t.Errorf("verdict changed with nil PagerOutcomes: got %s want %s", got, wantVerdict)
		}
	}
}
