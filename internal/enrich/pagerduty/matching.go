package pagerduty

import (
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/SaiPisey2/noisefloor/internal/enrich"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// matchIncidents correlates a rule's firing episodes to PagerDuty
// incidents.
//
// This is a BEST-EFFORT, TIME-AND-NAME heuristic, not a guaranteed
// dedup-key match: PagerDuty's REST API gives no field that reliably
// carries noisefloor's own episode fingerprint back (Alertmanager's
// pagerduty_configs receiver typically sets `dedup_key` from its own alert
// group fingerprint, not from the raw ALERTS series fingerprint this
// package reconstructs episodes from), so there is no exact join key
// available. Matching is therefore:
//
//  1. Candidate incidents are those whose title or incident_key contains
//     the rule's alert name (case-insensitive) at a delimiter-bounded
//     position -- true for PagerDuty integrations built from
//     Alertmanager's default templates, which include
//     {{ .CommonLabels.alertname }} in both. Delimiter-bounded, not a
//     plain substring test, because alert names routinely share a prefix
//     with a sibling's: DiskFull is a substring of DiskFullCritical,
//     HighLatency of HighLatencyP99, and every member of the
//     KubePodCrashLooping family absorbs its siblings under a plain
//     Contains. See wordBoundaryContains.
//  2. Among candidates, each episode is paired with the nearest
//     unmatched candidate whose created_at falls within
//     [episode.StartedAt - window, episode.EndedAt + window].
//  3. Matching is one-to-one: an incident matched to one episode is never
//     reused for another, and an episode with no candidate in range is
//     left unmatched -- exactly the partial-coverage case
//     enrich.Result.Matched documents. A rule this integration never
//     paged for legitimately matches nothing.
//
// Every field callers read off a match (Acknowledged, Escalated,
// HumanResolved, AutoResolved) is a REAL, OBSERVED fact about the matched
// incident once matched -- only the correlation step itself is a
// heuristic. See internal/score's PagerOutcomes.Coverage, which is exactly
// why a low-coverage result is deliberately weighted as weak evidence
// rather than trusted outright.
func matchIncidents(alertName string, episodes []store.Episode, incidents []incident, escalated map[string]bool, window time.Duration) []enrich.EpisodeOutcome {
	candidates := filterByAlertName(alertName, incidents)
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].createdAt.Before(candidates[j].createdAt)
	})

	sortedEpisodes := append([]store.Episode(nil), episodes...)
	sort.Slice(sortedEpisodes, func(i, j int) bool {
		return sortedEpisodes[i].StartedAt.Before(sortedEpisodes[j].StartedAt)
	})

	used := make([]bool, len(candidates))
	var out []enrich.EpisodeOutcome
	for _, ep := range sortedEpisodes {
		lo := ep.StartedAt.Add(-window)
		hi := ep.EndedAt.Add(window)

		best := -1
		for i, c := range candidates {
			if used[i] || c.createdAt.Before(lo) || c.createdAt.After(hi) {
				continue
			}
			if best == -1 || closer(c.createdAt, candidates[best].createdAt, ep.StartedAt) {
				best = i
			}
		}
		if best == -1 {
			continue
		}
		used[best] = true
		out = append(out, outcomeFrom(ep, candidates[best].incident, escalated))
	}
	return out
}

type candidate struct {
	incident
	createdAt time.Time
}

func filterByAlertName(alertName string, incidents []incident) []candidate {
	needle := strings.ToLower(alertName)
	if needle == "" {
		return nil
	}
	var out []candidate
	for _, inc := range incidents {
		if !wordBoundaryContains(strings.ToLower(inc.Title), needle) &&
			!wordBoundaryContains(strings.ToLower(inc.IncidentKey), needle) {
			continue
		}
		t, err := time.Parse(time.RFC3339, inc.CreatedAt)
		if err != nil {
			continue // an incident with an unparsable timestamp cannot be time-matched at all
		}
		out = append(out, candidate{incident: inc, createdAt: t})
	}
	return out
}

// wordBoundaryContains reports whether needle occurs in haystack at a
// delimiter-bounded position: the runes immediately before and after the
// match, if any, must not themselves be letters or digits. Both arguments
// are expected already lower-cased by the caller.
//
// A plain strings.Contains cross-attributes pages one way, short rule
// names absorbing longer ones that merely share a prefix -- DiskFull
// inside DiskFullCritical, HighLatency inside HighLatencyP99. Requiring a
// boundary on both sides rejects those while still matching the ordinary
// case, where the alert name is set off by spaces, brackets, underscores,
// or sits at the start or end of the string entirely (string edges count
// as boundaries, which is why they need no rune to compare against).
func wordBoundaryContains(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for offset := 0; offset < len(haystack); {
		idx := strings.Index(haystack[offset:], needle)
		if idx == -1 {
			return false
		}
		idx += offset
		end := idx + len(needle)

		beforeOK := idx == 0 || !isWordRune(lastRune(haystack[:idx]))
		afterOK := end == len(haystack) || !isWordRune(firstRune(haystack[end:]))
		if beforeOK && afterOK {
			return true
		}
		offset = idx + 1
	}
	return false
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

func closer(a, b, to time.Time) bool {
	da, db := a.Sub(to), b.Sub(to)
	if da < 0 {
		da = -da
	}
	if db < 0 {
		db = -db
	}
	return da < db
}

// outcomeFrom translates one matched PagerDuty incident into an
// enrich.EpisodeOutcome.
//
//   - Acknowledged: PagerDuty's documented contract is that
//     acknowledgements is non-empty exactly when a human (or, rarely,
//     another integration) explicitly acknowledged the incident.
//   - Escalated: from the account-wide log-entry sweep (see
//     logentries.go); nil escalated map means that sweep was unavailable,
//     in which case every incident reads Escalated: false -- an
//     understatement documented at the aggregate level (see
//     PagerOutcomes.EscalationRate's doc comment) rather than a fabricated
//     answer.
//   - HumanResolved / AutoResolved: PagerDuty does not expose a direct
//     "who/what resolved this" flag beyond last_status_change_by, so a
//     resolved incident whose last status change came from a
//     user_reference is read as human-resolved, and anything else
//     (service_reference, integration, or unset) as auto-resolved. This is
//     PagerDuty's own documented distinction for that field, applied
//     directly -- not a guess layered on top of it.
func outcomeFrom(ep store.Episode, inc incident, escalated map[string]bool) enrich.EpisodeOutcome {
	out := enrich.EpisodeOutcome{
		Fingerprint: ep.Fingerprint, StartedAt: ep.StartedAt, EndedAt: ep.EndedAt,
		Acknowledged: len(inc.Acknowledgements) > 0,
		ProviderRef:  inc.ID,
	}
	if escalated != nil {
		out.Escalated = escalated[inc.ID]
	}
	if inc.Status == "resolved" {
		if inc.LastStatusChangeBy.Type == "user_reference" {
			out.HumanResolved = true
		} else {
			out.AutoResolved = true
		}
	}
	return out
}
