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
	// flapWindow is how soon a re-fire on the same series counts as flapping.
	flapWindow = time.Hour
	// cofireWindow is how close two rules must fire to count as co-firing.
	cofireWindow = 2 * time.Minute
	// cofireMinOthers is how many other rules must co-fire before an episode
	// looks like it is riding someone else's incident.
	cofireMinOthers = 3

	businessStartHour = 9
	businessEndHour   = 18
)

type Signals struct {
	Fires              int
	UniqueFingerprints int
	P50Duration        time.Duration
	P90Duration        time.Duration

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
		if e.State == store.StatePending {
			pending = append(pending, e)
		} else {
			firing = append(firing, e)
		}
	}

	s := Signals{Fires: len(firing)}
	if len(firing) == 0 {
		s.PendingChurn = pendingChurn(pending, firing, in.Rule)
		return s
	}

	durations := make([]time.Duration, 0, len(firing))
	byFingerprint := map[string][]store.Episode{}
	var totalFiringSec, silencedSec, offhours, shortLived float64

	threshold := shortLivedThreshold(in.Rule)

	for _, e := range firing {
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
	s.OffhoursRate = offhours / n
	s.FlapRate = flapRate(byFingerprint, len(firing))
	s.CofireRatio = cofireRatio(firing, in.AllEpisodes, in.Rule.ID)
	s.Concentration = concentration(byFingerprint)
	s.PendingChurn = pendingChurn(pending, firing, in.Rule)

	if totalFiringSec > 0 {
		s.SilencedRate = math.Min(1, silencedSec/totalFiringSec)
	}
	return s
}

// shortLivedThreshold is the duration below which an episode had no time to be
// acted on. It scales with for:, because a rule that waits 10 minutes before
// firing is making a different claim about urgency than one that fires instantly.
func shortLivedThreshold(r store.Rule) time.Duration {
	if scaled := 3 * r.For; scaled > shortLivedFloor {
		return scaled
	}
	return shortLivedFloor
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
func cofireRatio(own, all []store.Episode, ruleID int64) float64 {
	if len(own) == 0 || len(all) == 0 {
		return 0
	}
	var cofired float64
	for _, e := range own {
		others := map[int64]bool{}
		for _, o := range all {
			if o.RuleID == ruleID {
				continue
			}
			if absDuration(o.StartedAt.Sub(e.StartedAt)) <= cofireWindow {
				others[o.RuleID] = true
			}
		}
		if len(others) >= cofireMinOthers {
			cofired++
		}
	}
	return cofired / float64(len(own))
}

// pendingChurn is the share of pending periods that never became a real alert.
// A high value means the threshold sits too close to normal operation.
func pendingChurn(pending, firing []store.Episode, r store.Rule) float64 {
	if len(pending) == 0 {
		return 0
	}
	tolerance := 2 * time.Minute
	if r.For > 0 {
		tolerance = r.For
	}

	var churned float64
	for _, p := range pending {
		became := false
		for _, f := range firing {
			gap := absDuration(f.StartedAt.Sub(p.EndedAt))
			if f.Fingerprint == p.Fingerprint && gap <= tolerance {
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

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
