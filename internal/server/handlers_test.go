package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"strconv"

	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}

func TestLeaderboardEmpty(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No scored rules yet") {
		t.Errorf("expected empty-state message, got:\n%s", rec.Body.String())
	}
}

func TestLeaderboardShowsScoredRule(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	id := seedRule(t, db, store.Rule{AlertName: "HighErrorRate", GroupName: "api", Active: true, For: 5 * time.Minute})
	seedEpisode(t, db, id, store.StateFiring, fixedNow.Add(-time.Hour), 2*time.Minute)
	seedScore(t, db, id, store.Score{
		Verdict: score.VerdictRetire, NoiseScore: 42, Confidence: 0.9,
		Signals: map[string]float64{"fires": 5, "short_lived_rate": 1, "p50_duration_s": 120},
	})

	rec := get(t, srv.Handler(), "/")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "HighErrorRate") {
		t.Errorf("leaderboard missing rule name:\n%s", body)
	}
	if !strings.Contains(body, "badge-retire") {
		t.Errorf("leaderboard missing verdict badge:\n%s", body)
	}
	if !strings.Contains(body, `href="/rules/`) {
		t.Errorf("leaderboard row is not linked to its detail page:\n%s", body)
	}
}

// TestLeaderboardShowsEstimatedVsMeasured pins the web UI surface of the
// distinction: an estimated row and a measured one must render visibly
// different badges, not the same "est"/generic marker either way.
func TestLeaderboardShowsEstimatedVsMeasured(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	est := seedRule(t, db, store.Rule{AlertName: "EstRule", GroupName: "g", Active: true})
	seedEpisode(t, db, est, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, est, store.Score{Verdict: score.VerdictRetire, NoiseScore: 45, Signals: map[string]float64{}})

	meas := seedRule(t, db, store.Rule{AlertName: "MeasRule", GroupName: "g", Active: true})
	seedEpisode(t, db, meas, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, meas, store.Score{
		Verdict: score.VerdictTune, NoiseScore: 45,
		Signals: map[string]float64{"measured": 1, "measured_coverage": 0.75},
	})

	body := get(t, srv.Handler(), "/").Body.String()
	if !strings.Contains(body, `class="badge evid-estimated"`) {
		t.Errorf("missing estimated evidence badge:\n%s", body)
	}
	if !strings.Contains(body, `class="badge evid-measured"`) {
		t.Errorf("missing measured evidence badge:\n%s", body)
	}
	if !strings.Contains(body, "measured (75% coverage)") {
		t.Errorf("missing measured coverage tooltip text:\n%s", body)
	}
}

func TestLeaderboardSort(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	low := seedRule(t, db, store.Rule{AlertName: "LowNoise", GroupName: "g", Active: true})
	seedEpisode(t, db, low, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, low, store.Score{Verdict: score.VerdictKeep, NoiseScore: 5, Signals: map[string]float64{}})

	high := seedRule(t, db, store.Rule{AlertName: "HighNoise", GroupName: "g", Active: true})
	seedEpisode(t, db, high, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, high, store.Score{Verdict: score.VerdictRetire, NoiseScore: 90, Signals: map[string]float64{}})

	// Default order: noise descending.
	body := get(t, srv.Handler(), "/").Body.String()
	if strings.Index(body, "HighNoise") > strings.Index(body, "LowNoise") {
		t.Errorf("default sort is not noise-descending:\n%s", body)
	}

	// Ascending: low noise first.
	body = get(t, srv.Handler(), "/?sort=noise&dir=asc").Body.String()
	if strings.Index(body, "LowNoise") > strings.Index(body, "HighNoise") {
		t.Errorf("sort=noise&dir=asc did not put LowNoise first:\n%s", body)
	}
}

func TestRuleDetailNotFound(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/rules/999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRuleDetailMalformedID(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	// "/rules/" (empty id segment) is deliberately not in this table: Go's
	// ServeMux does not match an empty {id} against "/rules/{id}" at all,
	// so it falls through to "/" (the leaderboard) instead of reaching this
	// handler -- a routing question, not this handler's own validation.
	for _, id := range []string{"abc", "1.5", "-", "%20"} {
		rec := get(t, srv.Handler(), "/rules/"+id)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id=%q: status = %d, want 400", id, rec.Code)
		}
	}
}

func TestRuleDetailNeverFired(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	id := seedRule(t, db, store.Rule{AlertName: "NeverFires", GroupName: "g", Active: true})

	rec := get(t, srv.Handler(), "/rules/"+strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "has no score") {
		t.Errorf("expected no-score message for an unfired rule:\n%s", rec.Body.String())
	}
}

func TestRuleDetailTuneShowsCounterfactual(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	id := seedRule(t, db, store.Rule{AlertName: "Flapper", GroupName: "g", Active: true, For: 30 * time.Second})
	start := fixedNow.Add(-24 * time.Hour)
	for i := 0; i < 15; i++ {
		seedEpisode(t, db, id, store.StateFiring, start.Add(time.Duration(i)*time.Hour), 3*time.Minute)
	}
	seedScore(t, db, id, store.Score{
		Verdict: score.VerdictTune, NoiseScore: 48, Confidence: 0.8,
		Signals: map[string]float64{
			"fires": 15, "flap_rate": 0.9, "short_lived_rate": 0.9,
			"p50_duration_s": 180, "p90_duration_s": 200,
		},
	})

	rec := get(t, srv.Handler(), "/rules/"+strconv.FormatInt(id, 10))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "Counterfactual") {
		t.Errorf("tune verdict should show a counterfactual section:\n%s", body)
	}
	if !strings.Contains(body, "would have suppressed") {
		t.Errorf("expected a counterfactual sentence:\n%s", body)
	}
	if !strings.Contains(body, "<svg") {
		t.Errorf("expected an episode timeline svg:\n%s", body)
	}
}

func TestCoverageEmpty(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/coverage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No coverage snapshot yet") {
		t.Errorf("expected empty-state message:\n%s", rec.Body.String())
	}
}

func TestCoverageGrid(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	snap := coverage.Snapshot{
		ComputedAt: fixedNow,
		Grid: []coverage.ServiceCoverage{{
			Service: coverage.Service{Name: "checkout", Source: "k8s", Traffic: 12.5, TrafficBasis: coverage.BasisRequests},
			Covered: map[coverage.Signal][]coverage.RuleMatch{
				coverage.SignalErrors: {{GroupName: "g", AlertName: "CheckoutErrors", Signal: coverage.SignalErrors, Certain: true, Scope: "matched", Reason: "matched on service label"}},
			},
		}, {
			Service: coverage.Service{Name: "search", Source: "job", Traffic: 40, TrafficBasis: coverage.BasisRequests},
			Covered: map[coverage.Signal][]coverage.RuleMatch{},
		}},
	}
	seedCoverageSnapshot(t, db, snap)

	rec := get(t, srv.Handler(), "/coverage")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "checkout") || !strings.Contains(body, "search") {
		t.Errorf("coverage grid missing a service:\n%s", body)
	}
	if !strings.Contains(body, "Blind spots") {
		t.Errorf("coverage page missing blind-spot section:\n%s", body)
	}
	if !strings.Contains(body, `class="cov-cell cov-matched-certain"`) {
		t.Errorf("expected a matched-certain cell for checkout/errors:\n%s", body)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/static/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "body") {
		t.Errorf("expected CSS content, got:\n%s", rec.Body.String())
	}
}
