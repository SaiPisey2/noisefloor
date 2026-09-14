package score

import (
	"math"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/config"
)

func defaultWeights() config.Weights { return config.Default().Weights }

func defaultConfidence() config.Confidence { return config.Default().Confidence }

// confident returns signals that clear the confidence floor, so verdict tests
// exercise verdict logic rather than the confidence gate.
func confident(s Signals) Signals {
	s.Fires = 100
	s.UniqueFingerprints = 5
	return s
}

func TestNoiseScoreIsZeroForCleanRule(t *testing.T) {
	if got := NoiseScore(Signals{}, defaultWeights()); got != 0 {
		t.Errorf("noise = %v, want 0", got)
	}
}

func TestNoiseScoreIsHundredForMaximallyNoisyRule(t *testing.T) {
	s := Signals{
		ShortLivedRate: 1, SilencedRate: 1, FlapRate: 1,
		CofireRatio: 1, OffhoursRate: 1,
	}
	if got := NoiseScore(s, defaultWeights()); math.Abs(got-100) > 0.001 {
		t.Errorf("noise = %v, want 100", got)
	}
}

func TestNoiseScoreIgnoresUnweightedSignals(t *testing.T) {
	s := Signals{Concentration: 1, PendingChurn: 1, P50Duration: time.Hour}
	if got := NoiseScore(s, defaultWeights()); got != 0 {
		t.Errorf("noise = %v, want 0; verdict-only signals must not move the score", got)
	}
}

func TestNoiseScoreAppliesWeights(t *testing.T) {
	s := Signals{ShortLivedRate: 1} // weight 0.30
	if got := NoiseScore(s, defaultWeights()); math.Abs(got-30) > 0.001 {
		t.Errorf("noise = %v, want 30", got)
	}
}

func TestConfidenceGrowsWithEpisodes(t *testing.T) {
	c := defaultConfidence() // min_episodes 10, min_window 14d
	window := 30 * 24 * time.Hour

	if got := Confidence(Signals{Fires: 0}, window, c); got != 0 {
		t.Errorf("confidence with 0 fires = %v, want 0", got)
	}
	if got := Confidence(Signals{Fires: 5}, window, c); math.Abs(got-0.5) > 0.001 {
		t.Errorf("confidence with 5 fires = %v, want 0.5", got)
	}
	if got := Confidence(Signals{Fires: 50}, window, c); got != 1 {
		t.Errorf("confidence with 50 fires = %v, want 1 (capped)", got)
	}
}

func TestConfidenceIsLimitedByShortWindow(t *testing.T) {
	c := defaultConfidence()
	// Plenty of fires, but only 7 days observed against a 14 day minimum.
	got := Confidence(Signals{Fires: 1000}, 7*24*time.Hour, c)
	if math.Abs(got-0.5) > 0.001 {
		t.Errorf("confidence = %v, want 0.5, limited by window", got)
	}
}

func TestVerdictKeepsWhenConfidenceIsLow(t *testing.T) {
	// Signals look terrible but only three episodes back them.
	s := Signals{Fires: 3, ShortLivedRate: 1, SilencedRate: 1}
	noise := NoiseScore(s, defaultWeights())
	conf := Confidence(s, 30*24*time.Hour, defaultConfidence())

	if got := Verdict(s, noise, conf); got != VerdictKeep {
		t.Errorf("verdict = %q, want %q; thin data must never propose a deletion", got, VerdictKeep)
	}
}

func TestVerdictRetiresSelfResolvingSilencedRule(t *testing.T) {
	s := confident(Signals{ShortLivedRate: 0.9, SilencedRate: 0.5, P50Duration: time.Minute})
	noise := NoiseScore(s, defaultWeights())
	if got := Verdict(s, noise, 1.0); got != VerdictRetire {
		t.Errorf("verdict = %q, want %q (noise %.1f)", got, VerdictRetire, noise)
	}
}

func TestVerdictTunesFlappingRuleRatherThanRetiringIt(t *testing.T) {
	// High noise, but the cause is flapping: the rule needs a longer for:,
	// not deletion.
	s := confident(Signals{ShortLivedRate: 0.9, FlapRate: 0.8, SilencedRate: 0.4})
	noise := NoiseScore(s, defaultWeights())
	if got := Verdict(s, noise, 1.0); got != VerdictTune {
		t.Errorf("verdict = %q, want %q (noise %.1f)", got, VerdictTune, noise)
	}
}

func TestVerdictTunesConcentratedRule(t *testing.T) {
	// The noise must clear noisyThreshold for this test to say anything about
	// concentration. Below it the correct verdict is `keep` regardless: a rule
	// nobody is suffering under does not need tuning just because its fires
	// happen to land on one host.
	s := confident(Signals{ShortLivedRate: 0.9, SilencedRate: 0.5, Concentration: 0.8})
	noise := NoiseScore(s, defaultWeights())
	if got := Verdict(s, noise, 1.0); got != VerdictTune {
		t.Errorf("verdict = %q, want %q", got, VerdictTune)
	}
}

func TestVerdictAutomatesFrequentRealAlerts(t *testing.T) {
	s := confident(Signals{ShortLivedRate: 0.05, P50Duration: 40 * time.Minute})
	noise := NoiseScore(s, defaultWeights())
	if got := Verdict(s, noise, 1.0); got != VerdictAutomate {
		t.Errorf("verdict = %q, want %q (noise %.1f)", got, VerdictAutomate, noise)
	}
}

func TestVerdictKeepsHealthyRule(t *testing.T) {
	s := confident(Signals{ShortLivedRate: 0.1, P50Duration: 5 * time.Minute})
	s.Fires = 12
	noise := NoiseScore(s, defaultWeights())
	if got := Verdict(s, noise, 1.0); got != VerdictKeep {
		t.Errorf("verdict = %q, want %q (noise %.1f)", got, VerdictKeep, noise)
	}
}

func TestEvaluateReturnsAllThree(t *testing.T) {
	cfg := config.Default()
	s := confident(Signals{ShortLivedRate: 0.9, SilencedRate: 0.5})

	noise, conf, verdict := Evaluate(s, 30*24*time.Hour, cfg)
	if noise <= 0 {
		t.Errorf("noise = %v, want > 0", noise)
	}
	if conf != 1 {
		t.Errorf("confidence = %v, want 1", conf)
	}
	if verdict != VerdictRetire {
		t.Errorf("verdict = %q, want %q", verdict, VerdictRetire)
	}
}

func TestVerdictAutomatesDespiteConcentration(t *testing.T) {
	// A concentrated but low-noise rule that is frequent, long-running and not
	// self-resolving still reaches `automate`. Concentration is evidence about
	// WHERE the fires come from, not a reason to withhold a runbook.
	s := confident(Signals{
		ShortLivedRate: 0.1, SilencedRate: 0.1, Concentration: 0.8,
		P50Duration: 20 * time.Minute,
	})
	noise := NoiseScore(s, defaultWeights())
	if noise >= noisyThreshold {
		t.Fatalf("fixture noise %.1f is not below the threshold; the test no "+
			"longer exercises the quiet-but-concentrated path", noise)
	}
	if got := Verdict(s, noise, 1.0); got != VerdictAutomate {
		t.Errorf("verdict = %q, want %q (noise %.1f)", got, VerdictAutomate, noise)
	}
}

// TestNoisyThresholdMatchesItsArchetypes re-derives the calibration in
// verdict.go from the weights, rather than trusting the comment. If a weight
// changes, or the offhours constant creeps back in, this fails with the
// arithmetic on show.
func TestNoisyThresholdMatchesItsArchetypes(t *testing.T) {
	w := defaultWeights()
	cases := []struct {
		name string
		s    Signals
		want float64
	}{
		{"retire: self-resolving and silenced",
			Signals{ShortLivedRate: 1, SilencedRate: 1}, 55},
		{"flapping: short episodes that re-fire",
			Signals{ShortLivedRate: 1, FlapRate: 1}, 50},
		{"cause: fires alongside everything",
			Signals{ShortLivedRate: 1, CofireRatio: 1}, 45},
		{"pure self-resolver: nothing else wrong",
			Signals{ShortLivedRate: 1}, 30},
		{"healthy: long, rare, unsilenced",
			Signals{ShortLivedRate: 0.05}, 1.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NoiseScore(c.s, w)
			if math.Abs(got-c.want) > 0.001 {
				t.Errorf("noise = %.2f, want %.2f", got, c.want)
			}
		})
	}
}

// TestPureSelfResolverIsExactlyActionable pins the reason noisyThreshold is
// 30: short_lived_rate alone carries 0.30 of the weight, so a rule whose every
// episode resolves before anyone could act scores exactly 30. That is the
// floor at which the clearest single pathology counts on its own, without
// needing a second one alongside it.
func TestPureSelfResolverIsExactlyActionable(t *testing.T) {
	s := confident(Signals{ShortLivedRate: 1, P50Duration: time.Minute})
	noise := NoiseScore(s, defaultWeights())
	if math.Abs(noise-float64(noisyThreshold)) > 0.001 {
		t.Fatalf("a pure self-resolver scores %.2f, but noisyThreshold is %d; "+
			"the threshold's stated rationale no longer holds",
			noise, noisyThreshold)
	}
	if got := Verdict(s, noise, 1.0); got != VerdictRetire {
		t.Errorf("verdict = %q, want %q", got, VerdictRetire)
	}
}

// TestUniformlyFiringRuleGetsNoOffHoursPenalty is the whole point of
// normalising the signal: a rule that fires round the clock used to collect
// ~7.3 points for the shape of the calendar alone.
func TestUniformlyFiringRuleGetsNoOffHoursPenalty(t *testing.T) {
	s := confident(Signals{OffhoursRate: normaliseOffHours(offHoursBaseline)})
	if got := NoiseScore(s, defaultWeights()); got != 0 {
		t.Errorf("noise = %.2f, want 0; firing at the uniform rate must cost "+
			"nothing", got)
	}
}

func TestVerdictBoundariesAreInclusive(t *testing.T) {
	// Every threshold below is documented as inclusive. Nothing else in the
	// suite exercises the exact values, so flipping any >= to > would pass.
	base := Signals{Fires: 100, UniqueFingerprints: 5}

	t.Run("confidence exactly at the gate", func(t *testing.T) {
		s := base
		s.ShortLivedRate, s.SilencedRate = 0.9, 0.5
		noise := NoiseScore(s, defaultWeights())
		if got := Verdict(s, noise, minConfidence); got == VerdictKeep {
			t.Errorf("confidence exactly %.2f was gated to keep; the gate is "+
				"documented as allowing it through", minConfidence)
		}
	})

	t.Run("noise exactly at the threshold", func(t *testing.T) {
		s := base
		if got := Verdict(s, noisyThreshold, 1.0); got != VerdictRetire {
			t.Errorf("noise exactly %.0f gave %q, want %q",
				float64(noisyThreshold), got, VerdictRetire)
		}
	})

	t.Run("flap rate exactly at the tune threshold", func(t *testing.T) {
		s := base
		s.FlapRate = flapTuneThreshold
		if got := Verdict(s, 10, 1.0); got != VerdictTune {
			t.Errorf("flap rate exactly %.2f gave %q, want %q",
				flapTuneThreshold, got, VerdictTune)
		}
	})

	t.Run("fires exactly at the automate minimum", func(t *testing.T) {
		s := Signals{Fires: automateMinFires, UniqueFingerprints: 2,
			ShortLivedRate: 0.1, P50Duration: automateMinDuration}
		if got := Verdict(s, 5, 1.0); got != VerdictAutomate {
			t.Errorf("fires exactly %d with duration exactly %v gave %q, want %q",
				automateMinFires, automateMinDuration, got, VerdictAutomate)
		}
	})
}
