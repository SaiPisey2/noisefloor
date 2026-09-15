package score

import "github.com/SaiPisey2/noisefloor/internal/enrich"

// PagerOutcomes summarizes real pager outcomes for a rule's episodes, as
// aggregated from an enrich.Result. A nil *PagerOutcomes (the default for
// every Signals value unless a caller explicitly supplies one via
// Input.PagerOutcomes) means no enricher was configured, or none of its
// data matched this rule -- every scoring path below treats that exactly
// like today's behavior, which is why a scan with no enricher configured
// (the demo, and every install before this feature) is byte-for-byte
// unaffected.
type PagerOutcomes struct {
	// Source names the enricher this came from (e.g. "pagerduty"), for
	// display only.
	Source string

	// Coverage is Matched / firing episode count for this rule, 0..1. Low
	// coverage means most of this rule's episodes still rest on inferred
	// signals; see measuredSufficient, which gates every scoring effect
	// below on a coverage AND sample-size floor so a handful of matched
	// pages cannot swing a verdict.
	Coverage float64
	// Matched is the absolute number of episodes a real outcome was found
	// for -- carried alongside Coverage because a rate alone cannot
	// distinguish "1 of 2 episodes matched" from "500 of 1000 matched".
	Matched int

	// AckRate is the share of Matched episodes a human acknowledged.
	AckRate float64
	// EscalationRate is the share of Matched episodes that escalated past
	// whoever the page first reached.
	EscalationRate float64
	// HumanResolvedRate and AutoResolvedRate are the share of Matched
	// episodes closed by a person versus by the source system observing
	// the condition clear on its own. Reported for display; scoring below
	// only reads AckRate and EscalationRate (engagementRate), since an
	// auto-resolve says nothing about whether a human ever looked.
	HumanResolvedRate float64
	AutoResolvedRate  float64

	// EscalationAvailable is false when the source could not fetch
	// escalation data at all for this window (e.g. PagerDuty's log-entry
	// sweep failed or was capped before finishing -- see
	// pagerduty.windowData.escalated). EscalationRate then reads 0 -- not
	// because nothing escalated, but because nobody could tell. Mirrors
	// internal/pr.Evidence.SilencesAvailable: asserting a rate from a
	// failed fetch presents an unmeasured number as a measured one, and
	// the two cases must stay distinguishable to a caller that gates a
	// verdict change on the rate reading low.
	EscalationAvailable bool
}

// engagementRate is the strongest single number PagerOutcomes offers for
// "did a human actually engage with this page": acknowledgement and
// escalation are both direct evidence a person was involved (escalation
// specifically means the FIRST responder didn't or couldn't handle it
// alone, which is still human engagement, just not captured as an
// acknowledgement), so this takes whichever rate is higher rather than
// requiring both or summing them past 1.
func (p *PagerOutcomes) engagementRate() float64 {
	if p == nil {
		return 0
	}
	if p.EscalationRate > p.AckRate {
		return p.EscalationRate
	}
	return p.AckRate
}

// AggregateOutcomes turns one enrich.Result into a *PagerOutcomes against a
// rule's own firing-episode count, or nil when there is nothing to report
// (no source name, or nothing matched) -- the same "absent means no
// measured evidence" contract PagerOutcomes documents.
func AggregateOutcomes(res enrich.Result, firingEpisodes int) *PagerOutcomes {
	if res.Source == "" || len(res.Matched) == 0 || firingEpisodes <= 0 {
		return nil
	}

	var acked, escalated, human, auto float64
	for _, m := range res.Matched {
		if m.Acknowledged {
			acked++
		}
		if m.Escalated {
			escalated++
		}
		if m.HumanResolved {
			human++
		}
		if m.AutoResolved {
			auto++
		}
	}
	n := float64(len(res.Matched))

	coverage := n / float64(firingEpisodes)
	if coverage > 1 {
		// A caller passed more matches than the rule has firing episodes --
		// duplicate matches, or a mismatched episode set. Clamp rather than
		// report an impossible >100% coverage; this is defensive, not an
		// expected path (internal/enrich/pagerduty's matcher never matches
		// one episode twice).
		coverage = 1
	}

	return &PagerOutcomes{
		Source: res.Source, Coverage: coverage, Matched: len(res.Matched),
		AckRate: acked / n, EscalationRate: escalated / n,
		HumanResolvedRate: human / n, AutoResolvedRate: auto / n,
		EscalationAvailable: res.EscalationAvailable,
	}
}
