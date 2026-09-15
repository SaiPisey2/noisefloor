package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/scanner"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

func TestAPIRulesShape(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	id := seedRule(t, db, store.Rule{
		AlertName: "HighLatency", GroupName: "api", Active: true, For: time.Minute,
		Labels: map[string]string{"severity": "page"},
	})
	seedEpisode(t, db, id, store.StateFiring, fixedNow.Add(-time.Hour), 2*time.Minute)
	seedScore(t, db, id, store.Score{
		Verdict: score.VerdictAutomate, NoiseScore: 10, Confidence: 0.9,
		Signals: map[string]float64{"fires": 30, "p50_duration_s": 900},
	})
	// A never-fired rule with no score: /api/rules must still list it, with
	// score omitted rather than a zero-valued lie about a verdict.
	seedRule(t, db, store.Rule{AlertName: "NeverFires", GroupName: "api", Active: true})

	rec := get(t, srv.Handler(), "/api/rules")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}

	var rows []struct {
		ID        int64             `json:"id"`
		AlertName string            `json:"alert_name"`
		GroupName string            `json:"group_name"`
		Labels    map[string]string `json:"labels"`
		Score     *struct {
			Verdict    string  `json:"verdict"`
			NoiseScore float64 `json:"noise_score"`
		} `json:"score"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rules, want 2", len(rows))
	}

	var scored, unscored bool
	for _, r := range rows {
		switch r.AlertName {
		case "HighLatency":
			scored = true
			if r.Score == nil {
				t.Fatalf("HighLatency: expected a score, got none")
			}
			if r.Score.Verdict != score.VerdictAutomate {
				t.Errorf("verdict = %q, want automate", r.Score.Verdict)
			}
			if r.Labels["severity"] != "page" {
				t.Errorf("labels = %v, missing severity=page", r.Labels)
			}
		case "NeverFires":
			unscored = true
			if r.Score != nil {
				t.Errorf("NeverFires: expected no score, got %+v", r.Score)
			}
		}
	}
	if !scored || !unscored {
		t.Fatalf("missing expected rows: scored=%v unscored=%v", scored, unscored)
	}
}

// TestAPIExposesMeasuredVsEstimated pins the API surface of the
// estimated/measured distinction (issue #14): a score backed by real
// pager outcomes must say so as a first-class field, not require a
// client to know score.Signals.Map's internal key names.
func TestAPIExposesMeasuredVsEstimated(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	estimated := seedRule(t, db, store.Rule{AlertName: "Estimated", GroupName: "g", Active: true})
	seedEpisode(t, db, estimated, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, estimated, store.Score{
		Verdict: score.VerdictRetire, NoiseScore: 45,
		Signals: map[string]float64{"fires": 10},
	})

	measured := seedRule(t, db, store.Rule{AlertName: "Measured", GroupName: "g", Active: true})
	seedEpisode(t, db, measured, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, measured, store.Score{
		Verdict: score.VerdictTune, NoiseScore: 45,
		Signals: map[string]float64{
			"fires": 10, "measured": 1, "measured_coverage": 0.92, "measured_ack_rate": 0.8,
		},
	})

	rec := get(t, srv.Handler(), "/api/scores")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rows []struct {
		AlertName        string  `json:"alert_name"`
		Measured         bool    `json:"measured"`
		MeasuredCoverage float64 `json:"measured_coverage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	byName := map[string]struct {
		Measured         bool
		MeasuredCoverage float64
	}{}
	for _, r := range rows {
		byName[r.AlertName] = struct {
			Measured         bool
			MeasuredCoverage float64
		}{r.Measured, r.MeasuredCoverage}
	}
	if byName["Estimated"].Measured {
		t.Error("Estimated row must report measured=false")
	}
	if !byName["Measured"].Measured {
		t.Error("Measured row must report measured=true")
	}
	if got := byName["Measured"].MeasuredCoverage; got != 0.92 {
		t.Errorf("measured_coverage = %v, want 0.92", got)
	}
}

func TestAPIRuleDetailShape(t *testing.T) {
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

	rec := get(t, srv.Handler(), "/api/rules/"+strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var detail struct {
		AlertName string `json:"alert_name"`
		Score     *struct {
			Verdict string `json:"verdict"`
		} `json:"score"`
		Episodes       []struct{} `json:"episodes"`
		Counterfactual *struct {
			Sentence     string  `json:"sentence"`
			CandidateFor string  `json:"candidate_for"`
			Suppressed   int     `json:"suppressed"`
			Suppressed2  float64 `json:"suppressed_rate"`
		} `json:"counterfactual"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if detail.AlertName != "Flapper" {
		t.Errorf("alert_name = %q", detail.AlertName)
	}
	if detail.Score == nil || detail.Score.Verdict != score.VerdictTune {
		t.Fatalf("score = %+v, want verdict=tune", detail.Score)
	}
	if len(detail.Episodes) != 15 {
		t.Errorf("episodes = %d, want 15", len(detail.Episodes))
	}
	if detail.Counterfactual == nil {
		t.Fatalf("expected a counterfactual for a tune verdict")
	}
	if detail.Counterfactual.Sentence == "" {
		t.Errorf("counterfactual sentence is empty")
	}
}

func TestAPIRuleDetailNotFound(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/api/rules/999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAPIRuleDetailMalformedID(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/api/rules/not-a-number")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAPIScoresShape(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	id := seedRule(t, db, store.Rule{AlertName: "R", GroupName: "g", Active: true})
	seedEpisode(t, db, id, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, id, store.Score{Verdict: score.VerdictRetire, NoiseScore: 77, Signals: map[string]float64{}})

	rec := get(t, srv.Handler(), "/api/scores")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var rows []struct {
		RuleID     int64   `json:"rule_id"`
		AlertName  string  `json:"alert_name"`
		Verdict    string  `json:"verdict"`
		NoiseScore float64 `json:"noise_score"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].RuleID != id || rows[0].AlertName != "R" || rows[0].Verdict != score.VerdictRetire || rows[0].NoiseScore != 77 {
		t.Errorf("row = %+v", rows[0])
	}
}

func TestAPICoverageShape(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	snap := coverage.Snapshot{
		ComputedAt: fixedNow,
		Grid: []coverage.ServiceCoverage{{
			Service: coverage.Service{Name: "checkout", Source: "k8s", Traffic: 12.5, TrafficBasis: coverage.BasisRequests},
			Covered: map[coverage.Signal][]coverage.RuleMatch{},
		}},
	}
	seedCoverageSnapshot(t, db, snap)

	rec := get(t, srv.Handler(), "/api/coverage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Grid []struct {
			Service struct{ Name string } `json:"service"`
		} `json:"grid"`
		BlindSpots []struct {
			Service struct{ Name string } `json:"service"`
		} `json:"blind_spots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(out.Grid) != 1 || out.Grid[0].Service.Name != "checkout" {
		t.Fatalf("grid = %+v", out.Grid)
	}
	if len(out.BlindSpots) != 1 || out.BlindSpots[0].Service.Name != "checkout" {
		t.Fatalf("blind_spots = %+v, want checkout (no coverage at all)", out.BlindSpots)
	}
}

func TestMetricsExposesScanAndVerdictSeries(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	id := seedRule(t, db, store.Rule{AlertName: "R", GroupName: "g", Active: true})
	seedEpisode(t, db, id, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, id, store.Score{Verdict: score.VerdictRetire, NoiseScore: 90, Signals: map[string]float64{}})

	err := scanner.SaveStats(context.Background(), db, scanner.ScanStats{
		ComputedAt: fixedNow, DurationSeconds: 1.5, Episodes: 3,
		RulesScored: 1, QueryFailures: 0,
		Verdicts: map[string]int{score.VerdictRetire: 1},
	})
	if err != nil {
		t.Fatalf("save scan stats: %v", err)
	}

	h := srv.Handler()
	get(t, h, "/healthz") // exercise the request middleware so http_requests_total has a sample

	rec := get(t, h, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"noisefloor_scan_duration_seconds",
		"noisefloor_scan_episodes_reconstructed",
		"noisefloor_scan_rules_scored",
		"noisefloor_scan_last_success_timestamp_seconds",
		"noisefloor_scan_query_failures",
		`noisefloor_rules_by_verdict{verdict="retire"} 1`,
		"noisefloor_http_requests_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q:\n%s", want, body)
		}
	}
}
