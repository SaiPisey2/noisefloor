// Package enrich defines the pager-enricher interface (issue #14): given a
// rule and a window, fetch the REAL outcome of its pages -- acknowledged,
// escalated, resolved by a human or automatically -- from a pager
// integration (PagerDuty, Opsgenie, incident.io), rather than inferring an
// outcome from how long an episode lasted.
//
// Every signal in internal/score today is a proxy inferred from firing
// shape: short_lived_rate assumes a short episode meant nobody could act,
// silenced_rate reads a human's silence as a judgement. Both are reasonable
// guesses. An enricher supplies the actual outcome, which is what turns a
// guess into a measurement -- see internal/score's PagerOutcomes for
// exactly how (and how conservatively) that measurement is allowed to move
// a verdict.
//
// Partial coverage is the normal case, not an error condition: not every
// alert pages (many rules never route to a pager at all), and not every
// page can be confidently correlated back to the episode that caused it.
// Result.Matched is therefore a SUBSET of a rule's firing episodes; callers
// must weigh a low-coverage result as weak evidence, never treat the
// uncovered remainder as "found and negative".
package enrich

import (
	"context"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

// EpisodeOutcome is one firing episode's real-world outcome, as reported by
// a pager integration.
type EpisodeOutcome struct {
	// Fingerprint and StartedAt/EndedAt identify which of the rule's
	// episodes this outcome belongs to -- the same episode a caller passed
	// into Enrich, echoed back rather than referenced by index, so a
	// caller can match by value without the enricher needing to know
	// store.Episode's own identity (its ID is a store-internal detail an
	// enricher should not need to round-trip).
	Fingerprint string
	StartedAt   time.Time
	EndedAt     time.Time

	// Acknowledged is true when a human acknowledged the page (PagerDuty:
	// a non-empty acknowledgements list; the general shape every pager
	// tool exposes in some form).
	Acknowledged bool
	// Escalated is true when the page was escalated to the next level of
	// an escalation policy -- evidence that whoever it first reached could
	// not, or did not, handle it alone.
	Escalated bool
	// HumanResolved is true when a person closed the incident, as opposed
	// to it clearing itself.
	HumanResolved bool
	// AutoResolved is true when the incident closed itself (the source
	// system observed the underlying condition clear, e.g. via its events
	// API), with no recorded human action.
	AutoResolved bool

	// ProviderRef is the source system's own identifier for whatever this
	// outcome came from (e.g. a PagerDuty incident ID). Carried for
	// traceability and debugging only -- nothing in scoring reads it.
	ProviderRef string
}

// Result is what one Enrich call returns for one rule's episodes over one
// window.
type Result struct {
	// Source names the enricher that produced this result (e.g.
	// "pagerduty"), for display -- see internal/score's PagerOutcomes.
	Source string
	// Matched is every episode this source could confidently correlate to
	// a real page outcome. It is a subset of the episodes passed into
	// Enrich, in no particular order, and may be empty: a rule this pager
	// integration never paged for, or paged for outside this source's
	// retention, legitimately has nothing to report.
	Matched []EpisodeOutcome

	// EscalationAvailable is false when this source could not fetch
	// escalation data at all for the window (a best-effort sweep that
	// failed or was capped before finishing), as opposed to fetching it
	// and finding no escalations. Every EpisodeOutcome.Escalated in
	// Matched then reads false regardless of the true value -- a
	// conservative degradation for most scoring, but not for every one
	// (see internal/score's PagerOutcomes and applyMeasuredOutcomes),
	// so callers that gate a decision on "essentially never escalated"
	// need to tell the two cases apart. Default false: an enricher that
	// never sets this is read as never having escalation data, which is
	// the safe assumption for one that does not implement escalation
	// detection at all.
	EscalationAvailable bool
}

// Enricher fetches real pager outcomes for one rule's episodes, from one
// external source.
//
// A source that covers only some of a rule's episodes -- the normal case,
// since not every alert pages -- represents that by returning a Result
// whose Matched is shorter than episodes, not by erroring or by inventing
// outcomes for episodes it has no evidence about.
type Enricher interface {
	// Name identifies this source for display (e.g. "pagerduty").
	Name() string

	// Enrich returns real outcomes for as many of episodes as this source
	// can identify, for rule, restricted to [since, until].
	//
	// An error means the FETCH failed (network, auth, a malformed
	// response) -- not that there was nothing to find, which is a Result
	// with an empty Matched and a nil error. Callers must degrade to
	// estimated scoring on error rather than abort: see
	// internal/scanner.Score, which prints a warning and proceeds exactly
	// as it does when Alertmanager is unreachable.
	Enrich(ctx context.Context, rule store.Rule, episodes []store.Episode, since, until time.Time) (Result, error)
}
