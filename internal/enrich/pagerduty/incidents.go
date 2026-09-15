package pagerduty

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// apiObject is PagerDuty's ubiquitous reference shape: every relationship
// (service, escalation policy, acknowledger, ...) is one of these. Field
// names match the official go-pagerduty client's APIObject.
type apiObject struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Summary string `json:"summary"`
}

// acknowledgement matches go-pagerduty's Acknowledgement: at least one
// entry on an incident means a human (or, per PagerDuty's docs, in rarer
// cases another service) explicitly acknowledged it.
type acknowledgement struct {
	At           string    `json:"at"`
	Acknowledger apiObject `json:"acknowledger"`
}

// incident is the subset of PagerDuty's documented Incident object this
// enricher reads. Field names and shapes match
// https://developer.pagerduty.com/api-reference (List/Get Incidents) and
// PagerDuty's own go-pagerduty client (github.com/PagerDuty/go-pagerduty,
// incident.go) as of 2026 -- this package has not been run against a live
// account, so it is possible a field has since changed shape; see the
// package doc comment.
type incident struct {
	ID                 string            `json:"id"`
	IncidentNumber     uint              `json:"incident_number"`
	Title              string            `json:"title"`
	CreatedAt          string            `json:"created_at"`
	IncidentKey        string            `json:"incident_key"`
	Service            apiObject         `json:"service"`
	Status             string            `json:"status"` // "triggered", "acknowledged", "resolved"
	Urgency            string            `json:"urgency"`
	Acknowledgements   []acknowledgement `json:"acknowledgements"`
	LastStatusChangeAt string            `json:"last_status_change_at"`
	LastStatusChangeBy apiObject         `json:"last_status_change_by"`
	ResolvedAt         string            `json:"resolved_at"`
}

type apiListMeta struct {
	Limit  int  `json:"limit"`
	Offset int  `json:"offset"`
	More   bool `json:"more"`
}

type listIncidentsResponse struct {
	apiListMeta
	Incidents []incident `json:"incidents"`
}

// listIncidents fetches every incident against serviceIDs whose created_at
// falls in [since, until], paginated at pageLimit and bounded by maxPages
// -- see that constant's doc comment for why a hard cap exists regardless
// of what the API's `more` flag says.
//
// This is the ONE bulk fetch per (since, until) window this enricher ever
// makes for incidents: Enricher caches the result and reuses it across
// every rule in a scan (see enricher.go), so a scan scoring hundreds of
// rules against thousands of combined pages still issues a bounded number
// of PagerDuty requests -- proportional to the window's total incident
// volume, not to the number of rules or episodes.
func (c *Client) listIncidents(ctx context.Context, serviceIDs []string, since, until time.Time) ([]incident, bool, error) {
	var out []incident
	truncated := false

	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q["service_ids[]"] = serviceIDs
		q.Set("since", since.UTC().Format(time.RFC3339))
		q.Set("until", until.UTC().Format(time.RFC3339))
		q.Set("date_range", "all") // PagerDuty otherwise clamps since/until to a 6-month default window
		q.Set("limit", strconv.Itoa(pageLimit))
		q.Set("offset", strconv.Itoa(page*pageLimit))
		q.Set("sort_by", "created_at:asc")

		var resp listIncidentsResponse
		if err := c.do(ctx, "/incidents", q, &resp); err != nil {
			return out, truncated, fmt.Errorf("list incidents: %w", err)
		}
		out = append(out, resp.Incidents...)
		if !resp.More {
			return out, truncated, nil
		}
	}
	// The loop ran maxPages times and the API still reports more: stop
	// here rather than fetch unboundedly. The caller gets everything
	// collected so far plus this flag, and degrades gracefully (lower
	// measured coverage, never an error) -- see AggregateOutcomes, which
	// already treats partial coverage as the normal case.
	return out, true, nil
}
