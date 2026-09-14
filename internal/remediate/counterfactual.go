package remediate

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Counterfactual is what a candidate `for:` value would have done to a
// rule's past firing episodes, stated precisely enough that a reviewer can
// check it by hand against the raw duration distribution.
//
// The arithmetic here is easy to get wrong by exactly one `for:`, so it is
// worth stating plainly: an episode's stored duration (store.Episode.
// Duration) measures how long the alert was FIRING. Prometheus does not
// start that clock until the underlying condition has already held
// continuously for the rule's CURRENT `for:` -- the episode begins only
// once the alert transitions from pending to firing. So the condition
// behind a firing episode of duration d, under `for: current`, actually
// held for `current + d`, not `d` alone. Raising `for:` to a candidate
// value suppresses an episode only when that full underlying duration --
// current + d -- falls short of the candidate. Comparing the candidate
// against d alone (forgetting to add current back in) would under-suppress
// every episode by exactly the current `for:`, silently, in a way that
// still looks like a plausible answer.
type Counterfactual struct {
	CurrentFor   time.Duration
	CandidateFor time.Duration

	Total      int // episodes considered
	Suppressed int // would never have fired at all under CandidateFor
	Retained   int // would still have fired

	// Delay is how much later every RETAINED episode's alert would have
	// started firing under CandidateFor: CandidateFor - CurrentFor. It is
	// one fixed number, not a per-episode average -- raising `for:` shifts
	// the firing instant by the same amount regardless of how long the
	// condition went on to hold afterwards, because the shift is in when
	// the pending timer is satisfied, not in the condition itself. Zero
	// when CandidateFor <= CurrentFor: nothing is delayed, and (per
	// Compute) nothing is suppressed either.
	Delay time.Duration

	// LongThreshold, LongTotal and LongRetained answer the part of the
	// design-doc sentence that guards against a tune proposal quietly
	// costing the episodes that mattered: of the episodes whose OBSERVED
	// firing duration was at least LongThreshold, how many are still
	// retained under CandidateFor?
	LongThreshold time.Duration
	LongTotal     int
	LongRetained  int
}

// SuppressedRate is Suppressed/Total, or 0 for an empty distribution.
func (c Counterfactual) SuppressedRate() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Suppressed) / float64(c.Total)
}

// Compute evaluates one candidate `for:` value against a rule's past firing
// durations (store.Episode.Duration values -- firing time only, which
// excludes the wait currentFor already imposed; see Counterfactual's doc
// comment for why that matters here).
//
// longThreshold decides which retained episodes count as "long enough to
// matter" in LongTotal/LongRetained; DefaultLongThreshold gives a
// reasonable value when the caller has no stronger opinion.
func Compute(durations []time.Duration, currentFor, candidateFor, longThreshold time.Duration) Counterfactual {
	c := Counterfactual{
		CurrentFor:    currentFor,
		CandidateFor:  candidateFor,
		Total:         len(durations),
		LongThreshold: longThreshold,
	}
	if candidateFor > currentFor {
		c.Delay = candidateFor - currentFor
	}

	for _, d := range durations {
		long := d >= longThreshold
		if long {
			c.LongTotal++
		}

		// The condition held for currentFor+d, not d -- see the package
		// doc comment on Counterfactual. An episode whose underlying
		// duration lands EXACTLY on candidateFor still fires (Prometheus
		// fires once the condition has held for at least `for:`), so the
		// suppression test is strict less-than, and that boundary episode
		// is retained with a firing duration of zero under the candidate.
		underlying := currentFor + d
		if underlying < candidateFor {
			c.Suppressed++
			continue
		}

		c.Retained++
		if long {
			c.LongRetained++
		}
	}
	return c
}

// DefaultLongThreshold is the "long enough to matter" bar Sentence uses
// when the caller supplies none of its own: twice the candidate `for:`.
//
// An episode retained at only just over the candidate is weak evidence --
// it could plausibly have landed on either side of suppression if the
// candidate had been chosen slightly differently. An episode that clears
// twice the candidate was unambiguously going to survive across any
// reasonable choice of `for:` in that neighbourhood, so counting it as
// "retained and long enough to matter" is a claim a reviewer can trust
// without re-deriving the boundary themselves.
func DefaultLongThreshold(candidateFor time.Duration) time.Duration {
	return 2 * candidateFor
}

// SelectCandidateFor proposes a `for:` value from a rule's past firing
// durations and its current `for:`.
//
// The heuristic is the rule's own P90 firing duration (score.Signals.
// P90Duration -- reserved for exactly this) plus the current `for:`.
// P90Duration is a firing duration, which -- per Counterfactual's doc
// comment -- already excludes the wait currentFor imposes, so the
// underlying condition behind the 90th-percentile episode actually held
// for P90Duration+currentFor. Proposing the candidate there suppresses
// roughly the shortest 90% of past episodes -- the bulk of the noise --
// while leaving the longest decile untouched, which is what "episodes long
// enough to matter" means for a rule whose problem is firing too eagerly
// rather than never firing at all.
//
// P90 over the median: the median would suppress roughly half of the
// genuinely long episodes along with the short noise, which is not a tune,
// it is a coin flip on which real incidents still page.
//
// P90 over P99: at the episode counts a rule accumulates in a scan window
// (tens to low thousands), P99 sits on one or two outlier episodes and
// swings by minutes depending on whether the most recent firing happened
// to run long -- an unstable proposal that changes between scans with no
// change in the rule's actual behaviour. P90 needs on the order of ten
// episodes to mean anything, which lines up with this project's own
// confidence.min_episodes floor (default 10, see internal/score/verdict.go)
// and stays put run to run once that floor is met.
//
// The result is never below currentFor: a firing duration is never
// negative, so this only ever proposes raising `for:`, never lowering it --
// which matches what a `tune` verdict driven by short-lived, flapping
// episodes is for.
func SelectCandidateFor(durations []time.Duration, currentFor time.Duration) time.Duration {
	if len(durations) == 0 {
		return currentFor
	}
	return currentFor + percentile(durations, 0.90)
}

// percentile mirrors internal/score's unexported percentile (same
// ceil-based rank), so the P90 this package derives agrees with the
// P90Duration signal already shown for the same rule in the report table
// and used to justify SelectCandidateFor above.
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

// Sentence renders a Counterfactual in the shape the design doc specifies:
//
//	p90 episode is 3m, current `for: 1m`; `for: 5m` would have suppressed
//	84% of past fires and retained 4 of 5 episodes longer than 10m.
//
// p90 is the rule's current P90Duration (score.Signals.P90Duration), shown
// so a reviewer can check CandidateFor against it directly -- it is the
// same number SelectCandidateFor derived its proposal from.
//
// A candidate at or below the current `for:` is called out explicitly
// rather than folded into the same sentence shape: Compute's arithmetic
// already makes it suppress nothing (see its doc comment), and a reviewer
// reading "suppressed 0%" for a LOWERED for: could easily misread that as
// "this change does nothing" rather than "this change makes no episode
// wait longer, so none were ever going to be suppressed".
func Sentence(p90 time.Duration, c Counterfactual) string {
	if c.CandidateFor <= c.CurrentFor {
		return fmt.Sprintf(
			"p90 episode is %s, current `for: %s`; `for: %s` is not longer than the current "+
				"value, so it would have suppressed nothing -- all %d episode(s) retained.",
			formatDuration(p90), formatDuration(c.CurrentFor), formatDuration(c.CandidateFor), c.Retained)
	}
	return fmt.Sprintf(
		"p90 episode is %s, current `for: %s`; `for: %s` would have suppressed %s of past fires "+
			"and retained %d of %d episodes longer than %s.",
		formatDuration(p90), formatDuration(c.CurrentFor), formatDuration(c.CandidateFor),
		formatPercent(c.SuppressedRate()), c.LongRetained, c.LongTotal, formatDuration(c.LongThreshold))
}

func formatPercent(rate float64) string {
	return fmt.Sprintf("%.0f%%", rate*100)
}

// formatDuration renders a duration compactly, the same shape
// internal/report uses for the P50 column (3m, 45s, 1h2m): every unit
// time.Duration.String would print gets noisy at episode-duration
// precision (3m0s, 1h2m0s), and this is going straight into prose a
// reviewer reads once.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)

	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	units := [3]struct {
		v int64
		u string
	}{{int64(h), "h"}, {int64(m), "m"}, {int64(s), "s"}}

	first := -1
	for i, u := range units {
		if u.v != 0 {
			first = i
			break
		}
	}
	if first == -1 {
		return "0s"
	}
	last := len(units) - 1
	for last > first && units[last].v == 0 {
		last--
	}

	out := ""
	for i := first; i <= last; i++ {
		out += fmt.Sprintf("%d%s", units[i].v, units[i].u)
	}
	return out
}
