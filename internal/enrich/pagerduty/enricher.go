package pagerduty

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/enrich"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// Enricher implements enrich.Enricher against PagerDuty's REST API v2.
type Enricher struct {
	client      *Client
	serviceIDs  []string
	matchWindow time.Duration

	mu    sync.Mutex
	cache map[windowKey]windowData
}

type windowKey struct{ since, until time.Time }

type windowData struct {
	incidents []incident
	// escalated is nil when the log-entries sweep failed or was capped by
	// maxPages before finishing -- see fetchWindow. A nil map still reads
	// as "not escalated" for everything (Go's nil-map read is false, never
	// a panic), which is the documented, conservative degradation: see
	// matching.go's outcomeFrom.
	escalated map[string]bool
}

// New builds a PagerDuty-backed enricher from configuration. hc may be
// nil (see Client's New).
func New(cfg config.PagerDuty, hc *http.Client) (*Enricher, error) {
	if !cfg.Configured() {
		return nil, fmt.Errorf("pagerduty: no service_ids configured")
	}
	return &Enricher{
		client:      NewClient(cfg, hc),
		serviceIDs:  cfg.ServiceIDs,
		matchWindow: cfg.MatchWindow.Std(),
		cache:       map[windowKey]windowData{},
	}, nil
}

var _ enrich.Enricher = (*Enricher)(nil)

func (e *Enricher) Name() string { return "pagerduty" }

// Enrich matches rule's firing episodes against PagerDuty incidents in
// [since, until]. See matching.go's doc comment for exactly how the
// correlation works and what it can and cannot guarantee.
//
// The underlying incident (and, best-effort, escalation) data for
// [since, until] is fetched from PagerDuty at most ONCE per Enricher,
// cached, and reused across every rule Enrich is called for with the same
// window -- which is every rule in one scan, since internal/scanner calls
// Enrich once per rule with the scan's own QueryStart/QueryEnd. A scan
// scoring hundreds of rules therefore issues a bounded number of PagerDuty
// requests, not one fetch per rule and never one per episode.
func (e *Enricher) Enrich(ctx context.Context, rule store.Rule, episodes []store.Episode, since, until time.Time) (enrich.Result, error) {
	data, err := e.fetchWindow(ctx, since, until)
	if err != nil {
		return enrich.Result{}, fmt.Errorf("pagerduty: %w", err)
	}

	var firing []store.Episode
	for _, ep := range episodes {
		if ep.State == store.StateFiring {
			firing = append(firing, ep)
		}
	}

	matched := matchIncidents(rule.AlertName, firing, data.incidents, data.escalated, e.matchWindow)
	return enrich.Result{
		Source: "pagerduty", Matched: matched,
		EscalationAvailable: data.escalated != nil,
	}, nil
}

func (e *Enricher) fetchWindow(ctx context.Context, since, until time.Time) (windowData, error) {
	key := windowKey{since, until}

	e.mu.Lock()
	if data, ok := e.cache[key]; ok {
		e.mu.Unlock()
		return data, nil
	}
	e.mu.Unlock()

	incidents, _, err := e.client.listIncidents(ctx, e.serviceIDs, since, until)
	if err != nil {
		return windowData{}, err
	}

	// Escalation detection is optional and best-effort -- see
	// listEscalatedIncidentIDs's doc comment. Its failure must not fail
	// the whole enrichment: acknowledgement data (the primary measured
	// signal) is already in hand.
	escalated, _, eerr := e.client.listEscalatedIncidentIDs(ctx, since, until)
	if eerr != nil {
		escalated = nil
	}

	data := windowData{incidents: incidents, escalated: escalated}
	e.mu.Lock()
	e.cache[key] = data
	e.mu.Unlock()
	return data, nil
}
