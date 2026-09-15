package pagerduty

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

// logEntry is the subset of PagerDuty's documented LogEntry object this
// enricher reads, to detect escalation. Field names match
// https://developer.pagerduty.com/api-reference (List Log Entries).
type logEntry struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"` // e.g. "escalate_log_entry", "acknowledge_log_entry"
	CreatedAt string    `json:"created_at"`
	Incident  apiObject `json:"incident"`
}

type listLogEntriesResponse struct {
	apiListMeta
	LogEntries []logEntry `json:"log_entries"`
}

// escalateLogEntryType is the log entry type PagerDuty records when an
// incident escalates to the next level of its escalation policy.
const escalateLogEntryType = "escalate_log_entry"

// listEscalatedIncidentIDs fetches the account-wide log entry stream for
// [since, until] ONCE (paginated, bounded by maxPages exactly like
// listIncidents) and returns the set of incident IDs that escalated at
// least once, rather than issuing one log-entries request per incident.
//
// Escalation detection is deliberately best-effort and OPTIONAL: it is not
// available in the base incident list response at all (unlike
// acknowledgement, which is), so it requires this second bulk fetch. A
// failure here (including hitting maxPages) must never fail the whole
// enrichment -- see Enricher.fetchWindow, which treats a non-nil error
// from this call as "escalation data unavailable", not as an enrichment
// failure: acknowledgement data, the primary measured signal, is already
// in hand from listIncidents by the time this runs.
func (c *Client) listEscalatedIncidentIDs(ctx context.Context, since, until time.Time) (map[string]bool, bool, error) {
	escalated := map[string]bool{}
	truncated := false

	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("since", since.UTC().Format(time.RFC3339))
		q.Set("until", until.UTC().Format(time.RFC3339))
		q.Set("limit", strconv.Itoa(pageLimit))
		q.Set("offset", strconv.Itoa(page*pageLimit))

		var resp listLogEntriesResponse
		if err := c.do(ctx, "/log_entries", q, &resp); err != nil {
			return escalated, truncated, err
		}
		for _, le := range resp.LogEntries {
			if le.Type == escalateLogEntryType && le.Incident.ID != "" {
				escalated[le.Incident.ID] = true
			}
		}
		if !resp.More {
			return escalated, truncated, nil
		}
	}
	return escalated, true, nil
}
