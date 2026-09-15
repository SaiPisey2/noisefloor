package pagerduty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

func testEnricher(t *testing.T, srv *httptest.Server) *Enricher {
	t.Helper()
	cfg := config.PagerDuty{
		BaseURL: srv.URL, ServiceIDs: []string{"PSERVICE1"},
		Auth: config.PagerDutyAuth{APIToken: "tok"}, MatchWindow: config.Duration(5 * time.Minute),
	}
	e, err := New(cfg, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestEnrichCachesTheBulkFetchAcrossRules guards the "a rule with
// thousands of pages must not issue thousands of requests" requirement at
// the level a scan actually calls it at: internal/scanner calls Enrich
// once PER RULE with the same [since, until] window, and this must not
// cost one incidents/log_entries fetch per rule.
func TestEnrichCachesTheBulkFetchAcrossRules(t *testing.T) {
	var incidentCalls, logCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/incidents":
			atomic.AddInt32(&incidentCalls, 1)
			w.Write([]byte(`{"limit":100,"offset":0,"more":false,"incidents":[]}`))
		case "/log_entries":
			atomic.AddInt32(&logCalls, 1)
			w.Write([]byte(`{"limit":100,"offset":0,"more":false,"log_entries":[]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	e := testEnricher(t, srv)
	rules := []store.Rule{{AlertName: "A"}, {AlertName: "B"}, {AlertName: "C"}}
	since, until := testWindow.since, testWindow.until
	for _, r := range rules {
		if _, err := e.Enrich(context.Background(), r, nil, since, until); err != nil {
			t.Fatalf("Enrich(%s): %v", r.AlertName, err)
		}
	}

	if incidentCalls != 1 {
		t.Errorf("incidents fetched %d times for 3 rules over the same window, want exactly 1", incidentCalls)
	}
	if logCalls != 1 {
		t.Errorf("log_entries fetched %d times for 3 rules over the same window, want exactly 1", logCalls)
	}
}

// TestEnrichDegradesWhenEscalationSweepFails pins that a failed
// log_entries fetch does not fail the whole enrichment: acknowledgement
// data is the primary measured signal and is already available from the
// incidents fetch alone.
func TestEnrichDegradesWhenEscalationSweepFails(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/incidents":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"limit":100,"offset":0,"more":false,"incidents":[{"id":"P1","title":"Flaky is firing","created_at":"` +
				base.Format(time.RFC3339) + `","acknowledgements":[{"acknowledger":{"type":"user_reference"}}]}]}`))
		case "/log_entries":
			w.Header().Set("Retry-After", "0") // keep the test's retry loop fast
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	e := testEnricher(t, srv)
	episodes := []store.Episode{mkEpisode("fp1", base, time.Minute)}
	res, err := e.Enrich(context.Background(), store.Rule{AlertName: "Flaky"}, episodes, testWindow.since, testWindow.until)
	if err != nil {
		t.Fatalf("Enrich must degrade rather than fail when only escalation data is unavailable: %v", err)
	}
	if len(res.Matched) != 1 || !res.Matched[0].Acknowledged {
		t.Fatalf("expected the acknowledged match to still be reported: %+v", res)
	}
	if res.Matched[0].Escalated {
		t.Error("escalation sweep failed -- Escalated must read false, not fabricate a value")
	}
}

// TestEnrichSurfacesIncidentsFetchFailure guards the other side: a failed
// incidents fetch (the primary data source) IS an error, which
// internal/scanner is responsible for degrading the whole rule to
// estimated scoring for -- Enrich itself must not silently swallow it.
func TestEnrichSurfacesIncidentsFetchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0") // keep the test's retry loop fast
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := testEnricher(t, srv)
	_, err := e.Enrich(context.Background(), store.Rule{AlertName: "A"}, nil, testWindow.since, testWindow.until)
	if err == nil {
		t.Fatal("want an error when the incidents fetch itself fails")
	}
}

func TestNewRefusesUnconfiguredPagerDuty(t *testing.T) {
	if _, err := New(config.PagerDuty{}, nil); err == nil {
		t.Fatal("want an error constructing an enricher with no service_ids configured")
	}
}
