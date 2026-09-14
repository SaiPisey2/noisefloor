// Package score turns stored episodes into explainable per-rule signals and a
// verdict. Every number here must be traceable to its inputs, because the
// output ends up in a pull request a human has to trust.
package score

import (
	"math"
	"sort"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

const (
	// shortLivedFloor is the minimum "nothing to do about it" threshold, used
	// when a rule has no for: clause.
	shortLivedFloor = 5 * time.Minute
	// shortLivedCap bounds that threshold. A human's response time does not
	// grow with a rule's debounce, so scaling without a ceiling would score a
	// five-hour episode on a `for: 2h` rule as too short to act on --
	// penalising precisely the rules whose authors already tuned them.
	shortLivedCap = 30 * time.Minute
	// flapWindow is how soon a re-fire on the same series counts as flapping.
	flapWindow = time.Hour
	// cofireWindow is how close two rules must fire to count as co-firing.
	cofireWindow = 2 * time.Minute
	// cofireMinOthers is how many other rules must co-fire before an episode
	// looks like it is riding someone else's incident.
	cofireMinOthers = 3

	businessStartHour = 9
	businessEndHour   = 18

	// offHoursBaseline is the share of an ordinary week that isOffHours calls
	// off-hours: 168 hours a week, minus 5 weekdays x 9 business hours
	// (09:00-18:00) = 45 on-hours, leaving 123. 123/168 = 0.732.
	//
	// That is what the RAW rate measures, and it is why the raw rate was
	// useless. Any rule firing round the clock scored ~0.73 -- so the signal
	// discriminated nothing between the noisiest rule and the healthiest one,
	// while adding a flat ~7.3 points to every noise score. The threshold had
	// been calibrated with that constant baked in.
	//
	// Rescaling against the baseline makes the signal measure what it was
	// always meant to: DISPROPORTIONATELY nocturnal firing. A uniformly-firing
	// rule scores 0, a rule that only ever fires at night scores 1.
	offHoursBaseline = 123.0 / 168.0
)

type Signals struct {
	Fires              int
	UniqueFingerprints int
	P50Duration        time.Duration
	P90Duration        time.Duration

	// FirstEpisode is when this rule's earliest firing episode in the window
	// started, or zero if it never fired. It is a confidence-only input, not a
	// scored signal: it bounds how long the rule has demonstrably existed, so a
	// rule added yesterday cannot claim a month of observation. It is
	// deliberately absent from Map, which stores the scored signals.
	FirstEpisode time.Time

	ShortLivedRate float64
	FlapRate       float64
	SilencedRate   float64
	OffhoursRate   float64
	CofireRatio    float64

	PendingChurn  float64
	Concentration float64
}

func (s Signals) Map() map[string]float64 {
	return map[string]float64{
		"fires":               float64(s.Fires),
		"unique_fingerprints": float64(s.UniqueFingerprints),
		"p50_duration_s":      s.P50Duration.Seconds(),
		"p90_duration_s":      s.P90Duration.Seconds(),
		"short_lived_rate":    s.ShortLivedRate,
		"flap_rate":           s.FlapRate,
		"silenced_rate":       s.SilencedRate,
		"offhours_rate":       s.OffhoursRate,
		"cofire_ratio":        s.CofireRatio,
		"pending_churn":       s.PendingChurn,
		"concentration":       s.Concentration,
	}
}

type Input struct {
	Rule        store.Rule
	Episodes    []store.Episode // this rule's episodes, firing and pending
	AllEpisodes []store.Episode // every rule's episodes, for co-fire detection
	Silences    []store.Silence
	Location    *time.Location
}

func Compute(in Input) Signals {
	loc := in.Location
	if loc == nil {
		loc = time.UTC
	}

	var firing, pending []store.Episode
	for _, e := range in.Episodes {
		switch e.State {
		case store.StateFiring:
			firing = append(firing, e)
		case store.StatePending:
			pending = append(pending, e)
		default:
			// An unknown state is not evidence of anything. Treating it as
			// firing would inflate Fires and the denominator of every scored
			// rate, quietly diluting all of them.
		}
	}

	s := Signals{Fires: len(firing)}
	if len(firing) == 0 {
		s.PendingChurn = pendingChurn(pending, firing)
		return s
	}

	durations := make([]time.Duration, 0, len(firing))
	byFingerprint := map[string][]store.Episode{}
	var totalFiringSec, silencedSec, offhours, shortLived float64

	threshold := shortLivedThreshold(in.Rule)

	for _, e := range firing {
		if s.FirstEpisode.IsZero() || e.StartedAt.Before(s.FirstEpisode) {
			s.FirstEpisode = e.StartedAt
		}

		d := e.Duration()
		durations = append(durations, d)
		byFingerprint[e.Fingerprint] = append(byFingerprint[e.Fingerprint], e)

		totalFiringSec += d.Seconds()
		silencedSec += collect.SilencedSeconds(
			e.StartedAt, e.EndedAt, in.Silences, in.Rule.AlertName, e.Labels)

		if d < threshold {
			shortLived++
		}
		if isOffHours(e.StartedAt.In(loc)) {
			offhours++
		}
	}

	n := float64(len(firing))
	s.UniqueFingerprints = len(byFingerprint)
	s.P50Duration = percentile(durations, 0.50)
	s.P90Duration = percentile(durations, 0.90)
	s.ShortLivedRate = shortLived / n
	s.OffhoursRate = normaliseOffHours(offhours / n)
	s.FlapRate = flapRate(byFingerprint, len(firing))
	s.CofireRatio = cofireRatio(firing, in.AllEpisodes, in.Rule.ID)
	s.Concentration = concentration(byFingerprint)
	s.PendingChurn = pendingChurn(pending, firing)

	if totalFiringSec > 0 {
		s.SilencedRate = math.Min(1, silencedSec/totalFiringSec)
	}
	return s
}

// shortLivedThreshold is the duration below which an episode had no time to be
// acted on. It scales mildly with for:, because a rule that waits 10 minutes
// before firing makes a different claim about urgency than one that fires
// instantly -- but it is bounded, because a responder's reaction time does not
// scale with the rule's debounce at all.
//
// The early return for a large For also guards 3*For, which overflows
// time.Duration past roughly 97 years and would silently wrap.
func shortLivedThreshold(r store.Rule) time.Duration {
	if r.For <= 0 {
		return shortLivedFloor
	}
	if r.For >= shortLivedCap {
		return shortLivedCap
	}
	switch scaled := 3 * r.For; {
	case scaled < shortLivedFloor:
		return shortLivedFloor
	case scaled > shortLivedCap:
		return shortLivedCap
	default:
		return scaled
	}
}

// normaliseOffHours rescales the raw off-hours share against what an
// indifferent rule would score, so the signal reports excess nocturnal firing
// rather than the shape of the working week. Below the baseline is not
// "negative night-time"; it is simply a rule that favours office hours, which
// carries no noise, so the result floors at 0.
func normaliseOffHours(raw float64) float64 {
	return math.Max(0, math.Min(1, (raw-offHoursBaseline)/(1-offHoursBaseline)))
}

func isOffHours(t time.Time) bool {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return true
	}
	h := t.Hour()
	return h < businessStartHour || h >= businessEndHour
}

func flapRate(byFingerprint map[string][]store.Episode, total int) float64 {
	if total == 0 {
		return 0
	}
	var refires float64
	for _, eps := range byFingerprint {
		sorted := append([]store.Episode(nil), eps...)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].StartedAt.Before(sorted[j].StartedAt)
		})
		for i := 1; i < len(sorted); i++ {
			if sorted[i].StartedAt.Sub(sorted[i-1].EndedAt) <= flapWindow {
				refires++
			}
		}
	}
	return refires / float64(total)
}

// cofireRatio measures how often this rule fires in lockstep with several
// others, which is what a cause-based alert riding a real incident looks like
// from the outside.
//
// Only FIRING episodes of other rules count. Nobody was paged for a pending
// alert, so a pending co-occurrence is not evidence that this rule rode
// someone else's incident.
//
// Comparison is on start times only, deliberately. An overlap test would mark
// every long-running alert as co-firing with everything that happened while it
// was open, which is the opposite of the signal wanted here.
func cofireRatio(own, all []store.Episode, ruleID int64) float64 {
	if len(own) == 0 || len(all) == 0 {
		return 0
	}

	others := make([]store.Episode, 0, len(all))
	for _, o := range all {
		if o.RuleID != ruleID && o.State == store.StateFiring {
			others = append(others, o)
		}
	}
	if len(others) == 0 {
		return 0
	}

	// Sort both once and sweep, rather than scanning every other episode for
	// every own episode. The naive form is O(own x all): at production scale
	// -- tens of thousands of episodes across a couple of hundred rules --
	// that is hundreds of millions of comparisons per scan.
	sort.Slice(others, func(i, j int) bool { return others[i].StartedAt.Before(others[j].StartedAt) })
	ownSorted := append([]store.Episode(nil), own...)
	sort.Slice(ownSorted, func(i, j int) bool { return ownSorted[i].StartedAt.Before(ownSorted[j].StartedAt) })

	var cofired float64
	lo := 0
	for _, e := range ownSorted {
		bandStart := e.StartedAt.Add(-cofireWindow)
		bandEnd := e.StartedAt.Add(cofireWindow)

		// ownSorted ascends, so the band's left edge only ever moves right.
		for lo < len(others) && others[lo].StartedAt.Before(bandStart) {
			lo++
		}

		distinct := map[int64]bool{}
		for i := lo; i < len(others) && !others[i].StartedAt.After(bandEnd); i++ {
			distinct[others[i].RuleID] = true
		}
		if len(distinct) >= cofireMinOthers {
			cofired++
		}
	}
	return cofired / float64(len(ownSorted))
}

// pendingChurn is the share of pending periods that never became a real alert.
// A high value means the threshold sits too close to normal operation.
//
// The tolerance is one sample step, not the rule's for:. A pending period
// becomes firing on the very next evaluation or not at all; allowing a
// for:-sized window would match a firing episode hours away and call it the
// same event, systematically under-reporting churn on long-for: rules.
//
// The gap is signed, and a firing episode starting BEFORE the pending period
// ended does not count: that is a different, already-open episode, not this
// pending period becoming real. One step of slack is allowed on each side
// only to absorb sampling jitter at the boundary.
func pendingChurn(pending, firing []store.Episode) float64 {
	if len(pending) == 0 {
		return 0
	}

	var churned float64
	for _, p := range pending {
		tolerance := p.Resolution
		if tolerance <= 0 {
			tolerance = time.Minute
		}

		became := false
		for _, f := range firing {
			if f.Fingerprint != p.Fingerprint {
				continue
			}
			if gap := f.StartedAt.Sub(p.EndedAt); gap >= -tolerance && gap <= tolerance {
				became = true
				break
			}
		}
		if !became {
			churned++
		}
	}
	return churned / float64(len(pending))
}

// concentration is the Gini coefficient over per-fingerprint episode counts.
// One bad host producing most of a rule's fires is a different problem from a
// rule that fires broadly, and it deserves a different verdict.
func concentration(byFingerprint map[string][]store.Episode) float64 {
	if len(byFingerprint) < 2 {
		return 0
	}
	counts := make([]float64, 0, len(byFingerprint))
	for _, eps := range byFingerprint {
		counts = append(counts, float64(len(eps)))
	}
	sort.Float64s(counts)

	n := float64(len(counts))
	var sum, weighted float64
	for i, c := range counts {
		sum += c
		weighted += float64(i+1) * c
	}
	if sum == 0 {
		return 0
	}
	g := (2*weighted)/(n*sum) - (n+1)/n

	// Small-sample correction. The uncorrected Gini over n positive counts
	// cannot exceed (n-1)/n, so a rule with two fingerprints would top out at
	// 0.50 however lopsided it is -- and the verdict logic thresholds
	// concentration at 0.60, which it could then never reach. Rescaling by
	// n/(n-1) puts the maximum at 1.0 for any n.
	if n > 1 {
		g *= n / (n - 1)
	}
	return math.Max(0, math.Min(1, g))
}

func percentile(durs []time.Duration, p float64) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
