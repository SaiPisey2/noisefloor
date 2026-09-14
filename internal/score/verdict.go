package score

import (
	"math"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/config"
)

const (
	VerdictRetire   = "retire"
	VerdictTune     = "tune"
	VerdictAutomate = "automate"
	VerdictKeep     = "keep"
)

const (
	// minConfidence is the floor below which no verdict other than keep is
	// allowed. Proposing changes on thin evidence is how a tool loses trust.
	minConfidence = 0.5
	// noisyThreshold is where a rule stops being merely imperfect.
	//
	// Well below 50, because no real rule is bad in all five scored dimensions
	// at once. flap_rate and cofire_ratio carry 0.35 of the weight between them
	// and are structurally mutually exclusive with the retire archetype: a rule
	// that self-resolves and gets silenced does not also flap. That 0.35 is
	// therefore unreachable for exactly the rules `retire` exists to catch, and
	// a threshold of 50 would demand a rule be bad in nearly every dimension
	// simultaneously -- which does not happen.
	//
	// Archetypes under the default weights (short 0.30, silenced 0.25,
	// flap 0.20, cofire 0.15, offhours 0.10), now that offhours_rate is
	// normalised against the working week and no longer adds a flat ~7.3 to
	// every rule:
	//
	//	retire   (self-resolving + silenced)   0.30 + 0.25          ~55
	//	flapping (short episodes, re-fires)    0.30 + 0.20          ~50
	//	cause    (fires alongside everything)  0.30 + 0.15          ~45
	//	self-resolver (short-lived, nothing else)     0.30          ~30
	//	healthy  (long, rare, unsilenced)                            ~2
	//
	// 30 is the floor at which a rule that ONLY ever self-resolves is
	// actionable: short_lived_rate alone carries 0.30 of the weight, so a rule
	// whose every episode resolves before anyone could act scores exactly 30
	// and nothing else about it needs to be wrong for that to be worth saying.
	// Anything higher would require a second pathology before the clearest
	// single one counts.
	noisyThreshold = 30

	flapTuneThreshold          = 0.3
	concentrationTuneThreshold = 0.6
	pendingChurnTuneThreshold  = 0.5

	automateMinFires    = 20
	automateMaxShort    = 0.3
	automateMinDuration = 15 * time.Minute
)

// NoiseScore combines the weighted signals into a 0..100 score. Only the five
// scored signals contribute; the rest inform the verdict instead.
func NoiseScore(s Signals, w config.Weights) float64 {
	weighted := w.ShortLivedRate*s.ShortLivedRate +
		w.SilencedRate*s.SilencedRate +
		w.FlapRate*s.FlapRate +
		w.CofireRatio*s.CofireRatio +
		w.OffhoursRate*s.OffhoursRate

	return math.Max(0, math.Min(100, 100*weighted))
}

// Confidence reports how much the evidence is worth, independent of how bad it
// looks. It is the weaker of two sufficiency measures: enough episodes, and
// enough observed time.
func Confidence(s Signals, window time.Duration, c config.Confidence) float64 {
	if c.MinEpisodes < 1 {
		c.MinEpisodes = 1
	}
	episodes := math.Min(1, float64(s.Fires)/float64(c.MinEpisodes))

	windowScore := 1.0
	if min := c.MinWindow.Std(); min > 0 {
		windowScore = math.Min(1, float64(window)/float64(min))
	}
	return math.Min(episodes, windowScore)
}

// Verdict decides what to do about a rule.
//
// Order matters. A flapping or concentrated rule is a threshold problem, so it
// is tuned rather than retired even when its noise score is high: deleting a
// rule that only needs a longer for: destroys real coverage.
func Verdict(s Signals, noise, confidence float64) string {
	if confidence < minConfidence {
		return VerdictKeep
	}

	switch {
	case s.FlapRate >= flapTuneThreshold,
		s.Concentration >= concentrationTuneThreshold,
		s.PendingChurn >= pendingChurnTuneThreshold:
		if noise >= noisyThreshold || s.FlapRate >= flapTuneThreshold {
			return VerdictTune
		}
	}

	if noise >= noisyThreshold {
		return VerdictRetire
	}

	// Concentration and pending churn deliberately do NOT block this arm. A
	// rule firing this often, this long, without self-resolving is a runbook
	// candidate whatever the shape of its distribution -- concentration tells
	// you where to aim the automation, not whether to write it. Both signals
	// are reported alongside the verdict, so the operator sees the skew.
	if s.Fires >= automateMinFires &&
		s.ShortLivedRate < automateMaxShort &&
		s.P50Duration >= automateMinDuration {
		return VerdictAutomate
	}

	return VerdictKeep
}

// Evaluate is the one call the rest of the program needs.
func Evaluate(s Signals, window time.Duration, cfg config.Config) (noise, confidence float64, verdict string) {
	noise = NoiseScore(s, cfg.Weights)
	confidence = Confidence(s, window, cfg.Confidence)
	verdict = Verdict(s, noise, confidence)
	return noise, confidence, verdict
}
