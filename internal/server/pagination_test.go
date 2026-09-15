package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

func TestAPIRuleDetailPaginatesEpisodes(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	id := seedRule(t, db, store.Rule{AlertName: "Flapper", GroupName: "g", Active: true, For: time.Minute})
	start := fixedNow.Add(-30 * 24 * time.Hour)
	const n = 30
	for i := 0; i < n; i++ {
		seedEpisode(t, db, id, store.StateFiring, start.Add(time.Duration(i)*time.Hour), time.Minute)
	}
	seedScore(t, db, id, store.Score{
		Verdict: score.VerdictKeep, NoiseScore: 5, Confidence: 0.9,
		Signals: map[string]float64{"fires": n},
	})

	type page struct {
		Episodes       []struct{} `json:"episodes"`
		EpisodesTotal  int        `json:"episodes_total"`
		EpisodesLimit  int        `json:"episodes_limit"`
		EpisodesOffset int        `json:"episodes_offset"`
	}

	rec := get(t, srv.Handler(), "/api/rules/"+strconv.FormatInt(id, 10)+"?limit=10&offset=0")
	var p1 page
	if err := json.Unmarshal(rec.Body.Bytes(), &p1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p1.Episodes) != 10 {
		t.Fatalf("page1 episodes = %d, want 10", len(p1.Episodes))
	}
	if p1.EpisodesTotal != n {
		t.Errorf("episodes_total = %d, want %d", p1.EpisodesTotal, n)
	}
	if p1.EpisodesLimit != 10 || p1.EpisodesOffset != 0 {
		t.Errorf("limit/offset = %d/%d, want 10/0", p1.EpisodesLimit, p1.EpisodesOffset)
	}

	rec = get(t, srv.Handler(), "/api/rules/"+strconv.FormatInt(id, 10)+"?limit=10&offset=20")
	var p3 page
	if err := json.Unmarshal(rec.Body.Bytes(), &p3); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p3.Episodes) != 10 {
		t.Fatalf("last page episodes = %d, want 10 (30 total, offset 20)", len(p3.Episodes))
	}

	// No pagination params at all: defaults to store.DefaultEpisodesPerPage,
	// comfortably above this rule's 30 episodes.
	rec = get(t, srv.Handler(), "/api/rules/"+strconv.FormatInt(id, 10))
	var pDefault page
	if err := json.Unmarshal(rec.Body.Bytes(), &pDefault); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pDefault.Episodes) != n {
		t.Errorf("default page episodes = %d, want all %d", len(pDefault.Episodes), n)
	}
}

// TestRuleDetailTimelineBucketsWhenEpisodesAreDense seeds a rule with more
// episodes than store.MaxTimelineEpisodes and checks the page switches to
// the bucketed summary, states that it did, and still renders a texture a
// human would read as flapping (multiple non-empty bars), rather than
// either one-rect-per-episode or a blank/empty timeline.
func TestRuleDetailTimelineBucketsWhenEpisodesAreDense(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	id := seedRule(t, db, store.Rule{AlertName: "VeryFlappy", GroupName: "g", Active: true, For: 30 * time.Second})
	start := fixedNow.Add(-30 * 24 * time.Hour)
	const n = store.MaxTimelineEpisodes + 500
	for i := 0; i < n; i++ {
		seedEpisode(t, db, id, store.StateFiring, start.Add(time.Duration(i)*time.Minute), 30*time.Second)
	}
	seedScore(t, db, id, store.Score{
		Verdict: score.VerdictTune, NoiseScore: 60, Confidence: 0.9,
		Signals: map[string]float64{"fires": float64(n), "p90_duration_s": 30},
	})

	rec := get(t, srv.Handler(), "/rules/"+strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "bucket summary") {
		t.Errorf("expected the page to say it is showing a bucketed summary:\n%s", body)
	}
	if !strings.Contains(body, strconv.Itoa(n)) {
		t.Errorf("expected the page to state the true episode total (%d) even though bucketed:\n%s", n, body)
	}
	if strings.Count(body, "<rect") < 2 {
		t.Errorf("expected multiple bucket bars communicating flapping, got:\n%s", body)
	}
	if strings.Count(body, "<rect") >= n {
		t.Errorf("expected far fewer bars than episodes once bucketed, got %d bars for %d episodes",
			strings.Count(body, "<rect"), n)
	}
}
