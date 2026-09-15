package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/pr"
	"github.com/SaiPisey2/noisefloor/internal/remediate"
	"github.com/SaiPisey2/noisefloor/internal/scanner"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// This file enforces the invariant rule_detail.html and dto.go both state
// in prose (see their doc comments): the rule-detail page's counterfactual
// is the same evidence a `noisefloor remediate` pull request would state
// for the same rule. Before the must-fix-1 fix, the page derived it from
// ListEpisodesForRule (every episode ever recorded) while internal/pr
// derives it from ListEpisodesInWindow (the scored window only) -- two
// different distributions that silently disagreed. This test seeds BOTH
// an in-window duration set and a batch of stale, out-of-window episodes
// (mirroring the reviewer's repro: old long episodes ninety days back),
// so it would fail if the page's window filtering ever regressed.

// parityWindowStart and parityWindowEnd are the scored window both the
// server rule and the pr.Build eval below share.
var (
	parityWindowStart = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	parityWindowEnd   = time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
)

// tuneMeLocation resolves internal/pr's own "TuneMe" fixture rule
// (internal/pr/testdata/fixture.yml), reused here rather than duplicated
// so this test exercises the real Build path -- including its drift
// check -- against a file that actually exists.
func tuneMeLocation(t *testing.T) remediate.RuleLocation {
	t.Helper()
	locs, ferrs, err := remediate.LocateRules("../pr/testdata")
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	if len(ferrs) != 0 {
		t.Fatalf("unexpected file errors: %v", ferrs)
	}
	loc, ok := remediate.LocationsByKey(locs)[remediate.RuleKey{Group: "fixture", AlertName: "TuneMe"}]
	if !ok {
		t.Fatalf("TuneMe location not found in ../pr/testdata")
	}
	return loc
}

// sentenceParagraph extracts the counterfactual-sentence paragraph
// (remediate.Sentence's output) from a rendered pr.Proposal body: TuneBody
// writes it as its own paragraph via "%s\n\n", so it is safe to find by
// splitting on blank lines and matching the sentence's fixed opening
// words.
func sentenceParagraph(t *testing.T, body string) string {
	t.Helper()
	for _, para := range strings.Split(body, "\n\n") {
		if strings.HasPrefix(para, "p90 episode is") {
			return para
		}
	}
	t.Fatalf("no counterfactual sentence found in proposal body:\n%s", body)
	return ""
}

func TestRuleDetailCounterfactualMatchesPRBot(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	// The same ten firing durations internal/pr's own TestBuildTuneProposal
	// uses for this exact fixture rule: seven 3m episodes, two 5m, one 15m.
	// P90 of these is 5m (300s).
	durations := []time.Duration{
		180 * time.Second, 180 * time.Second, 180 * time.Second, 180 * time.Second,
		180 * time.Second, 180 * time.Second, 180 * time.Second,
		300 * time.Second, 300 * time.Second,
		900 * time.Second,
	}

	id := seedRule(t, db, store.Rule{
		AlertName: "TuneMe", GroupName: "fixture", Active: true, For: time.Minute,
		Expr: "rate(errors_total[5m]) > 0",
	})
	for i, d := range durations {
		start := parityWindowStart.Add(time.Duration(i) * 24 * time.Hour)
		seedEpisode(t, db, id, store.StateFiring, start, d)
	}
	// Stale episodes well outside the scored window -- long enough (1h)
	// that including them would change the suppressed rate a lot. If the
	// page's window filter (must-fix 1) ever regressed to scanning every
	// episode ever recorded again, these would pull its counterfactual
	// away from the PR bot's and this test would catch it.
	staleStart := parityWindowStart.Add(-90 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		seedEpisode(t, db, id, store.StateFiring, staleStart.Add(time.Duration(i)*time.Hour), time.Hour)
	}

	seedScore(t, db, id, store.Score{
		WindowStart: parityWindowStart, WindowEnd: parityWindowEnd,
		Verdict: score.VerdictTune, NoiseScore: 51, Confidence: 0.85,
		Signals: map[string]float64{
			"fires": 10, "flap_rate": 0.5, "p50_duration_s": 180, "p90_duration_s": 300,
		},
	})

	// --- what the server states ---
	rec := get(t, srv.Handler(), "/api/rules/"+strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Counterfactual *struct {
			Sentence     string `json:"sentence"`
			CandidateFor string `json:"candidate_for"`
			Suppressed   int    `json:"suppressed"`
			Total        int    `json:"total"`
		} `json:"counterfactual"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if detail.Counterfactual == nil {
		t.Fatalf("expected a counterfactual for a tune verdict")
	}

	// --- what noisefloor remediate would state for the identical rule ---
	eval := scanner.RuleEval{
		Rule: store.Rule{
			GroupName: "fixture", AlertName: "TuneMe", Active: true, For: time.Minute,
			Expr: "rate(errors_total[5m]) > 0",
		},
		Location: tuneMeLocation(t), HasLocation: true,
		HasEpisodes: true,
		WindowStart: parityWindowStart, WindowEnd: parityWindowEnd,
		Verdict: score.VerdictTune, Noise: 51, Confidence: 0.85,
		Signals:           score.Signals{Fires: 10, P90Duration: 300 * time.Second, FlapRate: 0.5},
		SilencesAvailable: true,
		Durations:         durations, // window-only -- the stale episodes above are NOT here, matching ListEpisodesInWindow
	}
	proposal, refusal, err := pr.Build(eval)
	if err != nil {
		t.Fatalf("pr.Build: %v", err)
	}
	if refusal != nil {
		t.Fatalf("unexpected refusal: %v", refusal)
	}
	wantSentence := sentenceParagraph(t, proposal.Body)

	if detail.Counterfactual.Sentence != wantSentence {
		t.Errorf("page counterfactual sentence does not match the PR bot's:\n page: %q\n   pr: %q",
			detail.Counterfactual.Sentence, wantSentence)
	}
	if !strings.Contains(proposal.Title, "TuneMe") {
		t.Fatalf("sanity: proposal title = %q", proposal.Title) // guards against a vacuously-true refusal path
	}
}
