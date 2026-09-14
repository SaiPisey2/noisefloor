package score

import (
	"math"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/store"
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

// ObservedWindow is how long this rule has demonstrably been observable,
// bounded by the scan window. It is what confidence is measured against.
//
// Rule.FirstSeen is when noisefloor first met the rule, which on a first scan
// is `now` for every rule -- using it alone would zero every confidence on the
// first run and destroy the point of reconstructing history from ALERTS. The
// rule's earliest episode is the other half of the evidence: a rule met today
// whose episodes reach back a month demonstrably existed a month ago.
//
// So the clock starts at the earlier of the two. A genuinely new rule has both
// close to now and is correctly gated to a short window; an old rule seen for
// the first time keeps the full one.
func ObservedWindow(s Signals, r store.Rule, scanWindow time.Duration, now time.Time) time.Duration {
	start := r.FirstSeen
	if !s.FirstEpisode.IsZero() && (start.IsZero() || s.FirstEpisode.Before(start)) {
		start = s.FirstEpisode
	}
	if start.IsZero() {
		// Nothing known about the rule's age. The scan window is the only
		// honest bound available.
		return scanWindow
	}

	age := now.Sub(start)
	switch {
	case age < 0:
		// A clock skew, or a rule first seen in the future. Claim nothing.
		return 0
	case age < scanWindow:
		return age
	default:
		return scanWindow
	}
}

// Confidence reports how much the evidence is worth, independent of how bad it
// looks. It is the weaker of two sufficiency measures -- enough episodes, and
// enough observed time -- discounted by how concentrated those episodes are.
func Confidence(s Signals, window time.Duration, c config.Confidence) float64 {
	if c.MinEpisodes < 1 {
		c.MinEpisodes = 1
	}
	episodes := math.Min(1, float64(s.Fires)/float64(c.MinEpisodes))

	windowScore := 1.0
	if min := c.MinWindow.Std(); min > 0 {
		windowScore = math.Min(1, float64(window)/float64(min))
	}
	base := math.Min(episodes, windowScore)

	return base * fingerprintDiversityFactor(s)
}

// fingerprintDiversityFactor discounts confidence for fires concentrated on
// very few series. A rule stuck flapping on one host produced `fires`
// episodes but really only ever observed ONE thing behaving badly; the same
// fire count spread across many distinct series sampled the failure mode
// repeatedly and independently, which is stronger evidence for the same
// count. See docs/noisefloor-design.md, which lists unique_fingerprints as a
// confidence-only input.
//
// The discount is deliberately modest -- a factor between 0.8 and 1.0 -- for
// two reasons: concentration is also a legitimate pattern (a rule watching
// one global resource has nothing to spread across, and that is not weaker
// evidence about IT), and it is already reported on its own as the
// `concentration` signal, which routes a verdict rather than gating it. This
// only discounts confidence; it never zeroes it, so min_episodes and
// min_window remain the hard floors.
func fingerprintDiversityFactor(s Signals) float64 {
	if s.Fires == 0 {
		return 1
	}
	diversity := float64(s.UniqueFingerprints) / float64(s.Fires)
	return 0.8 + 0.2*diversity
}

// Verdict decides what to do about a rule.
//
// Order matters. A flapping or concentrated rule is a threshold problem, so it
// is tuned rather than retired even when its noise score is high: deleting a
// rule that only needs a longer for: destroys real coverage.
func Verdict(s Signals, noise, confidence float64, c config.Confidence) string {
	// min_episodes is a floor the user configured, and it must mean what it
	// says. Confidence is min(fires/min_episodes, window/min_window) against a
	// gate of 0.5, so relying on the ratio alone let `min_episodes: 10` hand
	// out a retire at 5 fires -- half the number the operator wrote down.
	// Compare the count directly as well.
	if s.Fires < c.MinEpisodes {
		return VerdictKeep
	}
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
//
// It takes the rule and `now` so the confidence window is bounded by the
// rule's own age here rather than at every call site, where it would
// eventually be forgotten. scanWindow is the span of history actually
// observed, which is not necessarily the span requested.
func Evaluate(s Signals, r store.Rule, scanWindow time.Duration, now time.Time, cfg config.Config) (noise, confidence float64, verdict string) {
	noise = NoiseScore(s, cfg.Weights)
	confidence = Confidence(s, ObservedWindow(s, r, scanWindow, now), cfg.Confidence)
	verdict = Verdict(s, noise, confidence, cfg.Confidence)
	return noise, confidence, verdict
}
