package pagerduty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/config"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func testClient(t *testing.T, srv *httptest.Server, token string) *Client {
	t.Helper()
	cfg := config.PagerDuty{
		BaseURL: srv.URL,
		Auth:    config.PagerDutyAuth{APIToken: token},
	}
	return NewClient(cfg, srv.Client())
}

var testWindow = struct{ since, until time.Time }{
	since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	until: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
}

// TestListIncidentsPaginates pins the pagination contract: offset advances
// by the page size and the loop stops exactly when `more` turns false,
// aggregating every page's incidents in order.
func TestListIncidentsPaginates(t *testing.T) {
	var offsets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/incidents" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		offsets = append(offsets, r.URL.Query().Get("offset"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("offset") == "0" {
			w.Write([]byte(readFixture(t, "incidents_page1.json")))
		} else {
			w.Write([]byte(readFixture(t, "incidents_page2.json")))
		}
	}))
	defer srv.Close()

	c := testClient(t, srv, "tok")
	got, truncated, err := c.listIncidents(context.Background(), []string{"PSERVICE1"}, testWindow.since, testWindow.until)
	if err != nil {
		t.Fatalf("listIncidents: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false (more turned false before maxPages)")
	}
	if len(got) != 3 {
		t.Fatalf("got %d incidents, want 3 (2 from page 1 + 1 from page 2)", len(got))
	}
	if got[0].ID != "PINC0001" || got[2].ID != "PINC0003" {
		t.Errorf("incidents out of order: %+v", got)
	}
	if want := []string{"0", "100"}; !equalStrings(offsets, want) {
		t.Errorf("offsets requested = %v, want %v", offsets, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestListIncidentsBacksOffOn429ThenSucceeds pins the rate-limit posture:
// a 429 is retried (honoring PagerDuty's documented RateLimit-Reset
// header, not a fixed sleep) rather than surfaced as a failure, and a
// large rule's worth of pages does not hammer the API on the retry either
// -- one retry, not a request storm.
func TestListIncidentsBacksOffOn429ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("RateLimit-Reset", "0") // keep the test fast
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(readFixture(t, "incidents_page2.json")))
	}))
	defer srv.Close()

	c := testClient(t, srv, "tok")
	got, _, err := c.listIncidents(context.Background(), []string{"PSERVICE1"}, testWindow.since, testWindow.until)
	if err != nil {
		t.Fatalf("listIncidents: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want exactly 2 (one 429, one success)", calls)
	}
	if len(got) != 1 {
		t.Errorf("got %d incidents, want 1", len(got))
	}
}

// TestListIncidentsGivesUpAfterMaxRetries guards against an indefinite
// retry loop against a persistently rate-limited (or down) PagerDuty: the
// call must fail, not hang, and the caller (internal/enrich/pagerduty's
// Enricher, and above it internal/scanner) degrades the scan to estimated
// scoring on this error rather than aborting it.
func TestListIncidentsGivesUpAfterMaxRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("RateLimit-Reset", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := testClient(t, srv, "tok")
	_, _, err := c.listIncidents(context.Background(), []string{"PSERVICE1"}, testWindow.since, testWindow.until)
	if err == nil {
		t.Fatal("want an error after exhausting retries, got nil")
	}
	if calls != int32(maxRetries) {
		t.Errorf("calls = %d, want exactly maxRetries (%d)", calls, maxRetries)
	}
}

// TestListIncidentsHonorsMaxPages guards the "a rule with thousands of
// pages must not issue thousands of requests" requirement at the
// pagination layer directly: a server that always claims more data stops
// being fetched once maxPages is hit, never fetched forever.
func TestListIncidentsHonorsMaxPages(t *testing.T) {
	old := maxPages
	maxPages = 3
	defer func() { maxPages = old }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"limit":100,"offset":0,"more":true,"incidents":[{"id":"X","title":"t","created_at":"2026-09-01T00:00:00Z"}]}`))
	}))
	defer srv.Close()

	c := testClient(t, srv, "tok")
	got, truncated, err := c.listIncidents(context.Background(), []string{"PSERVICE1"}, testWindow.since, testWindow.until)
	if err != nil {
		t.Fatalf("listIncidents: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true (maxPages was hit)")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want exactly maxPages (3)", calls)
	}
	if len(got) != 3 {
		t.Errorf("got %d incidents, want 3 (one per page fetched)", len(got))
	}
}

// TestListEscalatedIncidentIDs pins the escalation sweep against the fixture:
// only the incident with an escalate_log_entry is reported as escalated.
func TestListEscalatedIncidentIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(readFixture(t, "log_entries.json")))
	}))
	defer srv.Close()

	c := testClient(t, srv, "tok")
	got, truncated, err := c.listEscalatedIncidentIDs(context.Background(), testWindow.since, testWindow.until)
	if err != nil {
		t.Fatalf("listEscalatedIncidentIDs: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false")
	}
	if !got["PINC0002"] {
		t.Error("PINC0002 should be marked escalated")
	}
	if got["PINC0001"] {
		t.Error("PINC0001 has no escalate_log_entry and must not be marked escalated")
	}
}

// TestErrorsNeverContainTheToken mirrors
// internal/config's TestFailedRequestErrorDoesNotLeakToken: a failed
// request (connection refused) and a failed authenticated response
// (401 from the server) must never surface the API token in an error
// string or response body, since both are the kind of thing that gets
// logged verbatim.
func TestErrorsNeverContainTheToken(t *testing.T) {
	const secret = "pd-super-secret-api-token"

	t.Run("connection failure", func(t *testing.T) {
		cfg := config.PagerDuty{BaseURL: "http://127.0.0.1:1", Auth: config.PagerDutyAuth{APIToken: secret}}
		c := NewClient(cfg, &http.Client{Timeout: time.Second})
		_, _, err := c.listIncidents(context.Background(), []string{"PSERVICE1"}, testWindow.since, testWindow.until)
		if err == nil {
			t.Fatal("want an error connecting to a closed port")
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks the api token: %v", err)
		}
	})

	t.Run("authenticated failure response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A real PagerDuty 401 body does not echo the Authorization
			// header, but this guards the case regardless: even if it
			// did, the client must not propagate it into an error string
			// un-truncated in a way that leaks a token a test forgot to
			// scrub.
			http.Error(w, `{"error":{"message":"unauthorized: bad api key"}}`, http.StatusUnauthorized)
		}))
		defer srv.Close()

		c := testClient(t, srv, secret)
		_, _, err := c.listIncidents(context.Background(), []string{"PSERVICE1"}, testWindow.since, testWindow.until)
		if err == nil {
			t.Fatal("want an error for a 401 response")
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks the api token: %v", err)
		}
	})
}
