package pagerduty

import (
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

func mkEpisode(fp string, start time.Time, dur time.Duration) store.Episode {
	return store.Episode{Fingerprint: fp, StartedAt: start, EndedAt: start.Add(dur), State: store.StateFiring}
}

func mkIncident(id, title, createdAt string) incident {
	return incident{ID: id, Title: title, CreatedAt: createdAt}
}

// TestMatchIncidentsPartialCoverage pins the interface's central design
// requirement: a source that covers only SOME of a rule's episodes must be
// representable. Two episodes fire; PagerDuty only has an incident for one
// of them.
func TestMatchIncidentsPartialCoverage(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	episodes := []store.Episode{
		mkEpisode("fp1", base, time.Minute),
		mkEpisode("fp2", base.Add(time.Hour), time.Minute),
	}
	incidents := []incident{
		mkIncident("P1", "DemoSpiky is firing", base.Add(30*time.Second).Format(time.RFC3339)),
		// nothing for the second episode
	}

	got := matchIncidents("DemoSpiky", episodes, incidents, nil, 5*time.Minute)
	if len(got) != 1 {
		t.Fatalf("matched %d episodes, want exactly 1 (partial coverage)", len(got))
	}
	if got[0].Fingerprint != "fp1" {
		t.Errorf("matched fingerprint = %s, want fp1", got[0].Fingerprint)
	}
}

// TestMatchIncidentsFiltersByAlertName guards against cross-rule
// contamination: an incident whose title names a different alert must
// never be matched to this rule's episode, even if it falls inside the
// time window.
func TestMatchIncidentsFiltersByAlertName(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	episodes := []store.Episode{mkEpisode("fp1", base, time.Minute)}
	incidents := []incident{
		mkIncident("P1", "DemoCauseA is firing", base.Format(time.RFC3339)),
	}
	got := matchIncidents("DemoSpiky", episodes, incidents, nil, 5*time.Minute)
	if len(got) != 0 {
		t.Errorf("matched %d, want 0 (different alert name)", len(got))
	}
}

// TestMatchIncidentsRequiresWordBoundary is finding 4: strings.Contains
// cross-attributes pages whose title or incident_key merely embeds this
// rule's name inside a longer identifier with no delimiter between them --
// DiskFull inside DiskFullCritical, HighLatency inside HighLatencyP99. The
// shorter rule must not absorb the longer one's incidents.
func TestMatchIncidentsRequiresWordBoundary(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	episodes := []store.Episode{mkEpisode("fp1", base, time.Minute)}
	incidents := []incident{
		mkIncident("P1", "[FIRING] DiskFullCritical on db-1", base.Format(time.RFC3339)),
	}
	got := matchIncidents("DiskFull", episodes, incidents, nil, 5*time.Minute)
	if len(got) != 0 {
		t.Errorf("matched %d, want 0: DiskFullCritical is a different alert than DiskFull, "+
			"not the same one with a suffix", len(got))
	}
}

// TestMatchIncidentsWordBoundaryAllowsRealDelimiters is the control for
// the test above: a rule name set off by ordinary punctuation, brackets,
// or an underscore must still match -- only the no-delimiter absorption
// case is rejected.
func TestMatchIncidentsWordBoundaryAllowsRealDelimiters(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	episodes := []store.Episode{mkEpisode("fp1", base, time.Minute)}
	incidents := []incident{
		mkIncident("P1", "[FIRING] DiskFull on db-1", base.Format(time.RFC3339)),
	}
	got := matchIncidents("DiskFull", episodes, incidents, nil, 5*time.Minute)
	if len(got) != 1 {
		t.Errorf("matched %d, want 1: DiskFull is delimiter-bounded by brackets and spaces", len(got))
	}

	incidentKey := []incident{
		{ID: "P2", Title: "unrelated title", IncidentKey: "DiskFull_db-1_1700000000", CreatedAt: base.Format(time.RFC3339)},
	}
	got = matchIncidents("DiskFull", []store.Episode{mkEpisode("fp2", base, time.Minute)}, incidentKey, nil, 5*time.Minute)
	if len(got) != 1 {
		t.Errorf("matched %d, want 1: DiskFull is delimiter-bounded by an underscore in incident_key", len(got))
	}
}

// TestMatchIncidentsRespectsWindow guards the time-proximity bound: an
// incident that matches by name but falls outside the configured match
// window must not be matched.
func TestMatchIncidentsRespectsWindow(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	episodes := []store.Episode{mkEpisode("fp1", base, time.Minute)}
	incidents := []incident{
		mkIncident("P1", "DemoSpiky is firing", base.Add(time.Hour).Format(time.RFC3339)),
	}
	got := matchIncidents("DemoSpiky", episodes, incidents, nil, 5*time.Minute)
	if len(got) != 0 {
		t.Errorf("matched %d, want 0 (incident is an hour outside the match window)", len(got))
	}
}

// TestMatchIncidentsAreOneToOne guards against double counting: two
// episodes must never be credited to the same incident, and vice versa --
// each candidate is used at most once.
func TestMatchIncidentsAreOneToOne(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	episodes := []store.Episode{
		mkEpisode("fp1", base, time.Minute),
		mkEpisode("fp2", base.Add(2*time.Minute), time.Minute),
	}
	// Only one candidate incident, positioned so it is technically closest
	// to fp1 but still within window of fp2 too.
	incidents := []incident{
		mkIncident("P1", "DemoSpiky is firing", base.Add(30*time.Second).Format(time.RFC3339)),
	}
	got := matchIncidents("DemoSpiky", episodes, incidents, nil, 5*time.Minute)
	if len(got) != 1 {
		t.Fatalf("matched %d, want exactly 1 (one incident cannot cover two episodes)", len(got))
	}
	if got[0].Fingerprint != "fp1" {
		t.Errorf("matched to %s, want fp1 (nearer in time)", got[0].Fingerprint)
	}
}

// TestOutcomeFromReadsAcknowledgementAndResolution pins the field mapping
// from PagerDuty's documented incident shape to enrich.EpisodeOutcome.
func TestOutcomeFromReadsAcknowledgementAndResolution(t *testing.T) {
	ep := mkEpisode("fp1", time.Now(), time.Minute)

	acked := incident{
		ID: "P1", Status: "resolved",
		Acknowledgements:   []acknowledgement{{Acknowledger: apiObject{Type: "user_reference"}}},
		LastStatusChangeBy: apiObject{Type: "user_reference"},
	}
	out := outcomeFrom(ep, acked, map[string]bool{"P1": true})
	if !out.Acknowledged || !out.HumanResolved || out.AutoResolved || !out.Escalated {
		t.Errorf("acked+human-resolved+escalated incident: got %+v", out)
	}

	autoResolved := incident{
		ID: "P2", Status: "resolved",
		LastStatusChangeBy: apiObject{Type: "service_reference"},
	}
	out = outcomeFrom(ep, autoResolved, nil)
	if out.Acknowledged || out.HumanResolved || !out.AutoResolved || out.Escalated {
		t.Errorf("unacknowledged, auto-resolved incident with no escalation data: got %+v", out)
	}
}
