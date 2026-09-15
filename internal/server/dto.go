package server

import (
	"fmt"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/remediate"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// --- JSON API shapes ---------------------------------------------------
//
// These mirror the store's own types closely (Rule, Score, Episode) rather
// than inventing a parallel vocabulary: the API answers "what did the scan
// produce", not a redesigned model of it.

type ruleAPI struct {
	ID          int64             `json:"id"`
	AlertName   string            `json:"alert_name"`
	GroupName   string            `json:"group_name"`
	File        string            `json:"file,omitempty"`
	Line        int               `json:"line,omitempty"`
	Expr        string            `json:"expr"`
	For         string            `json:"for"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Active      bool              `json:"active"`
	FirstSeen   time.Time         `json:"first_seen"`
	LastSeen    time.Time         `json:"last_seen"`
	Score       *scoreAPI         `json:"score,omitempty"`
}

type scoreAPI struct {
	WindowStart time.Time          `json:"window_start"`
	WindowEnd   time.Time          `json:"window_end"`
	Verdict     string             `json:"verdict"`
	NoiseScore  float64            `json:"noise_score"`
	Confidence  float64            `json:"confidence"`
	Signals     map[string]float64 `json:"signals"`
	ComputedAt  time.Time          `json:"computed_at"`

	// Measured and MeasuredCoverage restate signals["measured"] and
	// signals["measured_coverage"] (see score.Signals.Map) as first-class
	// fields, rather than requiring an API consumer to know those key
	// names -- this row's verdict rests on real pager outcomes
	// (issue #14) exactly this much, as opposed to being inferred purely
	// from firing shape. False/0 for every score computed without a pager
	// enricher configured, which is every score before this feature.
	Measured         bool    `json:"measured"`
	MeasuredCoverage float64 `json:"measured_coverage,omitempty"`
}

func newScoreAPI(sc store.Score) *scoreAPI {
	return &scoreAPI{
		WindowStart: sc.WindowStart, WindowEnd: sc.WindowEnd,
		Verdict: sc.Verdict, NoiseScore: sc.NoiseScore, Confidence: sc.Confidence,
		Signals: sc.Signals, ComputedAt: sc.ComputedAt,
		Measured: sc.Signals["measured"] != 0, MeasuredCoverage: sc.Signals["measured_coverage"],
	}
}

func newRuleAPI(r store.Rule, sc *store.Score) ruleAPI {
	out := ruleAPI{
		ID: r.ID, AlertName: r.AlertName, GroupName: r.GroupName,
		File: r.File, Line: r.Line, Expr: r.Expr, For: r.For.String(),
		Labels: r.Labels, Annotations: r.Annotations, Active: r.Active,
		FirstSeen: r.FirstSeen, LastSeen: r.LastSeen,
	}
	if sc != nil {
		out.Score = newScoreAPI(*sc)
	}
	return out
}

type episodeAPI struct {
	StartedAt time.Time         `json:"started_at"`
	EndedAt   time.Time         `json:"ended_at"`
	State     string            `json:"state"`
	Labels    map[string]string `json:"labels"`
}

func newEpisodeAPI(e store.Episode) episodeAPI {
	return episodeAPI{StartedAt: e.StartedAt, EndedAt: e.EndedAt, State: e.State, Labels: e.Labels}
}

type silenceAPI struct {
	CreatedBy string    `json:"created_by"`
	Comment   string    `json:"comment"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
}

func newSilenceAPI(s store.Silence) silenceAPI {
	return silenceAPI{CreatedBy: s.CreatedBy, Comment: s.Comment, StartsAt: s.StartsAt, EndsAt: s.EndsAt}
}

// counterfactualAPI mirrors remediate.Counterfactual, plus the prose
// sentence a human reads on the rule-detail page -- the same evidence a
// remediation PR body would state, computed the same way (see
// internal/remediate/counterfactual.go), so the page and a PR never
// disagree about what a candidate for: would have done.
type counterfactualAPI struct {
	CurrentFor    string  `json:"current_for"`
	CandidateFor  string  `json:"candidate_for"`
	Total         int     `json:"total"`
	Suppressed    int     `json:"suppressed"`
	Retained      int     `json:"retained"`
	SuppressedPct float64 `json:"suppressed_rate"`
	LongThreshold string  `json:"long_threshold"`
	LongTotal     int     `json:"long_total"`
	LongRetained  int     `json:"long_retained"`
	Sentence      string  `json:"sentence"`
}

func newCounterfactualAPI(p90 time.Duration, cf remediate.Counterfactual) counterfactualAPI {
	return counterfactualAPI{
		CurrentFor: cf.CurrentFor.String(), CandidateFor: cf.CandidateFor.String(),
		Total: cf.Total, Suppressed: cf.Suppressed, Retained: cf.Retained,
		SuppressedPct: cf.SuppressedRate(), LongThreshold: cf.LongThreshold.String(),
		LongTotal: cf.LongTotal, LongRetained: cf.LongRetained,
		Sentence: remediate.Sentence(p90, cf),
	}
}

type ruleDetailAPI struct {
	ruleAPI
	// Episodes is one page of this rule's episodes, most recent first --
	// bounded by EpisodesLimit/EpisodesOffset (see episodePaginationParams
	// and store.ListEpisodesForRule), never the rule's whole history. A
	// flapping rule can accumulate tens of thousands of episodes; without
	// this, one request for one rule could allocate on the order of 100MB.
	// EpisodesTotal is the true count (store.CountEpisodesForRule) a
	// client pages through with `?limit=` and `?offset=`.
	Episodes       []episodeAPI       `json:"episodes"`
	EpisodesTotal  int                `json:"episodes_total"`
	EpisodesLimit  int                `json:"episodes_limit"`
	EpisodesOffset int                `json:"episodes_offset"`
	Silences       []silenceAPI       `json:"silences,omitempty"`
	Counterfactual *counterfactualAPI `json:"counterfactual,omitempty"`
}

type scoreRowAPI struct {
	RuleID    int64  `json:"rule_id"`
	AlertName string `json:"alert_name"`
	GroupName string `json:"group_name"`
	scoreAPI
}

type coverageAPI struct {
	ComputedAt   time.Time                  `json:"computed_at"`
	Grid         []coverage.ServiceCoverage `json:"grid"`
	BlindSpots   []coverage.BlindSpot       `json:"blind_spots"`
	ParseErrors  map[string]string          `json:"parse_errors,omitempty"`
	Unattributed []string                   `json:"unattributed,omitempty"`
	ExcludedJobs []string                   `json:"excluded_jobs,omitempty"`
}

func newCoverageAPI(snap coverage.Snapshot) coverageAPI {
	return coverageAPI{
		ComputedAt: snap.ComputedAt, Grid: snap.Grid,
		BlindSpots:  coverage.RankBlindSpots(snap.Grid),
		ParseErrors: snap.ParseErrors, Unattributed: snap.Unattributed,
		ExcludedJobs: snap.ExcludedJobs,
	}
}

// --- HTML view models ---------------------------------------------------

// leaderboardRow is one row of the leaderboard table: a scored rule and
// every signal that drives its verdict, formatted for display. Mirrors
// report.Row/Render's column set exactly, so the table on screen matches
// the table `noisefloor scan` prints to a terminal.
type leaderboardRow struct {
	ID         int64
	AlertName  string
	GroupName  string
	Verdict    string
	Noise      float64
	Confidence float64
	Fires      int
	P50        string
	Short      string
	Silenced   string
	Flap       string
	Cofire     string
	Conc       string
	Churn      string
	Night      string

	// Measured and Evidence mirror report.Render's EVID column and
	// scoreAPI's Measured/MeasuredCoverage fields (see their doc
	// comments): the same estimated-vs-measured distinction, rendered the
	// same way, in every surface. Evidence is "estimated" or
	// "measured (NN% coverage)"; Measured is the plain boolean the
	// template uses to pick a badge class.
	Measured bool
	Evidence string
}

func newLeaderboardRow(r store.Rule, sc store.Score) leaderboardRow {
	s := sc.Signals
	row := leaderboardRow{
		ID: r.ID, AlertName: r.AlertName, GroupName: r.GroupName,
		Verdict: sc.Verdict, Noise: sc.NoiseScore, Confidence: sc.Confidence,
		Fires:    intOf(s, "fires"),
		P50:      formatDuration(durationOf(s, "p50_duration_s")),
		Short:    pctOf(s, "short_lived_rate"),
		Silenced: pctOf(s, "silenced_rate"),
		Flap:     pctOf(s, "flap_rate"),
		Cofire:   pctOf(s, "cofire_ratio"),
		Conc:     pctOf(s, "concentration"),
		Churn:    pctOf(s, "pending_churn"),
		Night:    pctOf(s, "offhours_rate"),
	}
	row.Measured = s["measured"] != 0
	if row.Measured {
		row.Evidence = fmt.Sprintf("measured (%s coverage)", pctOf(s, "measured_coverage"))
	} else {
		row.Evidence = "estimated"
	}
	return row
}

// signalRow is one line of the rule-detail page's signal breakdown table:
// a signal's raw value, its scoring weight (zero for the verdict/
// confidence-only signals), and its contribution to the noise score --
// the arithmetic behind the headline number, laid out so a sceptic can
// re-add it by hand.
type signalRow struct {
	Name         string
	Value        string
	Weight       float64
	Contribution float64
	Weighted     bool
}

// ruleDetail is the rule-detail page's whole view model. It deliberately
// does not carry a raw episode list -- see loadRuleDetail's doc comment --
// only EpisodesTotal, the exact count a paginated caller states alongside
// its own bounded page.
type ruleDetail struct {
	Rule                   store.Rule
	Score                  store.Score
	HasScore               bool
	Signals                []signalRow
	EpisodesTotal          int
	Timeline               timeline
	Silences               []store.Silence
	Counterfactual         *remediate.Counterfactual
	CounterfactualSentence string
	P90                    time.Duration

	// Measured and Evidence are the same estimated-vs-measured statement
	// as the leaderboard's row and the API's scoreAPI, rendered as one
	// sentence for the detail page's banner -- see leaderboardRow's doc
	// comment. Empty/false when there is no score at all (HasScore false).
	Measured bool
	Evidence string
}

// columnHeader is one sortable leaderboard column header.
type columnHeader struct {
	Label  string
	URL    string
	Sorted bool
	Dir    string // "asc" or "desc", arrow-rendered by the template
}

// --- top-level page data (wraps a view model with nav state) -----------

type leaderboardPage struct {
	Page    string
	Rows    []leaderboardRow
	Columns []columnHeader
}

type ruleDetailPage struct {
	Page   string
	Detail ruleDetail
}

// coverageCell is one grid cell: a signal's coverage state for one
// service, with the same certainty/scope priority coverage.Render's
// terminal table uses (matched-certain > global-certain > matched-guess >
// global-guess > none) so the page never shows a different answer than the
// CLI for the same snapshot.
type coverageCell struct {
	Glyph   string
	Class   string // CSS class suffix: matched-certain, global-certain, matched-guess, global-guess, none
	Tooltip string
}

type coverageRow struct {
	Service coverage.Service
	Cells   []coverageCell
}

type coveragePage struct {
	Page        string
	HasSnapshot bool
	Snapshot    coverage.Snapshot
	Rows        []coverageRow
	BlindSpots  []coverage.BlindSpot
}
