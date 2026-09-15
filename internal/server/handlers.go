package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/SaiPisey2/noisefloor/internal/collect"
	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/remediate"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// --- error rendering -----------------------------------------------------

func (s *Server) notFound(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusNotFound)
}

func (s *Server) badRequest(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusBadRequest)
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	// The database is local and read-only from this process's point of
	// view; a failure here is an operational problem (a locked file, a
	// missing database), never untrusted input, so the error text is safe
	// to show as-is.
	http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		// Too late to change the status code once ExecuteTemplate has
		// started writing, but this at least reaches the server's own log.
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// --- leaderboard ----------------------------------------------------------

// sortableColumns maps a `sort` query value to a comparator over
// leaderboardRow. Every entry is a real column on the page; anything else
// in the query string falls back to the default (noise, descending).
// FIRES and the percentage columns are shown but not sortable through
// this mechanism -- see columnOrder.
var sortableColumns = map[string]func(a, b leaderboardRow) bool{
	"noise":      func(a, b leaderboardRow) bool { return a.Noise < b.Noise },
	"confidence": func(a, b leaderboardRow) bool { return a.Confidence < b.Confidence },
	"verdict":    func(a, b leaderboardRow) bool { return a.Verdict < b.Verdict },
	"group":      func(a, b leaderboardRow) bool { return a.GroupName < b.GroupName },
	"rule":       func(a, b leaderboardRow) bool { return a.AlertName < b.AlertName },
}

// columnOrder is display order for the sortable header row, paired with
// its label. The remaining columns (FIRES, P50, and the percentage
// signals) are static headers in the template -- sorting every column
// this table has would be a lot of machinery for little benefit over
// sorting the ones that actually decide the verdict.
var columnOrder = []struct{ key, label string }{
	{"noise", "NOISE"}, {"confidence", "CONF"}, {"verdict", "VERDICT"},
	{"group", "GROUP"}, {"rule", "RULE"},
}

func (s *Server) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rules, err := s.db.ListRules(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	scores, err := s.db.ListLatestScores(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	byRule := make(map[int64]store.Rule, len(rules))
	for _, ru := range rules {
		byRule[ru.ID] = ru
	}

	var rows []leaderboardRow
	for _, sc := range scores {
		ru, ok := byRule[sc.RuleID]
		if !ok {
			continue
		}
		rows = append(rows, newLeaderboardRow(ru, sc))
	}

	sortKey := r.URL.Query().Get("sort")
	less, ok := sortableColumns[sortKey]
	if !ok {
		sortKey, less = "noise", sortableColumns["noise"]
	}
	dir := r.URL.Query().Get("dir")
	if dir != "asc" {
		dir = "desc"
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if dir == "desc" {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})

	var cols []columnHeader
	for _, c := range columnOrder {
		nextDir := "desc"
		arrow := "↓" // down arrow: default direction shown when this column is not yet active
		if c.key == sortKey {
			arrow = "↓"
			if dir == "desc" {
				nextDir, arrow = "asc", "↓"
			} else {
				nextDir, arrow = "desc", "↑"
			}
		}
		cols = append(cols, columnHeader{
			Label:  c.label,
			URL:    "/?sort=" + c.key + "&dir=" + nextDir,
			Sorted: c.key == sortKey,
			Dir:    arrow,
		})
	}

	s.render(w, "leaderboard_page", leaderboardPage{Page: "leaderboard", Rows: rows, Columns: cols})
}

// --- rule detail ------------------------------------------------------

// loadRuleDetail builds the rule-detail page's whole view model, EXCEPT
// the paginated raw episode list the JSON API states -- see
// handleAPIRuleDetail, which fetches its own bounded page separately. This
// keeps the shape both the HTML page and the JSON API share (rule, score,
// signals, timeline, silences, counterfactual) built exactly once, without
// forcing the HTML page (which never renders a raw episode list, only the
// SVG timeline) to hold one in memory just because the API needs one.
func (s *Server) loadRuleDetail(ctx context.Context, id int64) (ruleDetail, bool, error) {
	rule, ok, err := s.db.GetRule(ctx, id)
	if err != nil || !ok {
		return ruleDetail{}, ok, err
	}

	total, err := s.db.CountEpisodesForRule(ctx, id)
	if err != nil {
		return ruleDetail{}, true, err
	}
	detail := ruleDetail{Rule: rule, EpisodesTotal: total}

	// A rule with more episodes than store.MaxTimelineEpisodes gets a
	// bucketed summary computed entirely in SQL (O(bucket count) memory)
	// instead of one store.Episode -- labels map, fingerprint string and
	// all -- fetched and held per episode. See store.EpisodeTimelineSummary
	// and store.MaxTimelineEpisodes's doc comments for why the threshold
	// sits where it does.
	if total > store.MaxTimelineEpisodes {
		buckets, bucketWidth, windowStart, windowEnd, _, err := s.db.EpisodeTimelineSummary(ctx, id, s.nowFunc(), store.TimelineBuckets)
		if err != nil {
			return ruleDetail{}, true, err
		}
		detail.Timeline = newBucketedTimeline(buckets, bucketWidth, total, windowStart, windowEnd, s.nowFunc())
	} else {
		episodes, err := s.db.ListEpisodesForRule(ctx, id, store.MaxTimelineEpisodes, 0)
		if err != nil {
			return ruleDetail{}, true, err
		}
		detail.Timeline = newTimeline(episodes, s.nowFunc())
	}

	sc, hasScore, err := s.db.GetLatestScore(ctx, id)
	if err != nil {
		return ruleDetail{}, true, err
	}
	detail.HasScore = hasScore
	if !hasScore {
		return detail, true, nil
	}
	detail.Score = sc
	detail.Signals = buildSignalRows(sc.Signals, s.cfg.Weights)

	// The window a stored score was actually computed over, not this
	// rule's entire history: internal/pr's tune proposal for this same
	// rule derives its counterfactual from exactly this set (see
	// internal/scanner.RuleEval.Durations, filled from
	// ListEpisodesInWindow), so building it from anything wider -- as this
	// used to, from every episode ever recorded -- let the page propose a
	// candidate `for:` a `noisefloor remediate` run for the same rule
	// would never make.
	//
	// Two separate queries, not one: CoveringSilences needs full episodes
	// (their labels, to match a silence's matchers), which is the
	// expensive shape to fetch at scale, while the counterfactual only
	// needs durations. Capping the cheap query far higher than the
	// expensive one keeps a dense window's counterfactual accurate without
	// paying for tens of thousands of label maps just to compute it -- see
	// store.MaxWindowEpisodes and store.MaxWindowDurations.
	firing, err := s.db.ListFiringEpisodesForRuleInWindow(ctx, id, sc.WindowStart, sc.WindowEnd)
	if err != nil {
		return ruleDetail{}, true, err
	}

	silences, err := s.db.ListSilences(ctx, sc.WindowStart, sc.WindowEnd)
	if err != nil {
		return ruleDetail{}, true, err
	}
	detail.Silences = collect.CoveringSilences(rule.AlertName, firing, silences)

	if sc.Verdict == score.VerdictTune {
		durations, err := s.db.ListFiringDurationsForRuleInWindow(ctx, id, sc.WindowStart, sc.WindowEnd)
		if err != nil {
			return ruleDetail{}, true, err
		}
		if len(durations) > 0 {
			// The PR bot's own formula (internal/pr's buildTune): current
			// `for:` plus the rule's P90 firing duration, taken from the
			// signal a scan already persisted rather than by re-deriving a
			// P90 from this handler's own (possibly differently-windowed,
			// now fixed but worth keeping self-consistent) duration slice
			// with remediate.SelectCandidateFor. Same input, same formula,
			// same answer -- see TestRuleDetailCounterfactualMatchesPRBot.
			p90 := durationOf(sc.Signals, "p90_duration_s")
			candidate := rule.For + p90
			cf := remediate.Compute(durations, rule.For, candidate, remediate.DefaultLongThreshold(candidate))
			detail.Counterfactual = &cf
			detail.CounterfactualSentence = remediate.Sentence(p90, cf)
			detail.P90 = p90
		}
	}

	return detail, true, nil
}

func (s *Server) handleRuleDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.badRequest(w, "malformed rule id")
		return
	}
	detail, found, err := s.loadRuleDetail(r.Context(), id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.notFound(w, "no such rule")
		return
	}
	s.render(w, "rule_page", ruleDetailPage{Page: "rule", Detail: detail})
}

// --- coverage --------------------------------------------------------

func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	snap, ok, err := coverage.Load(r.Context(), s.db)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !ok {
		s.render(w, "coverage_page", coveragePage{Page: "coverage", HasSnapshot: false})
		return
	}

	rows := make([]coverageRow, 0, len(snap.Grid))
	for _, sc := range snap.Grid {
		row := coverageRow{Service: sc.Service}
		for _, sig := range coverage.Signals {
			row.Cells = append(row.Cells, cellFor(sc.Covered[sig]))
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Service.Name < rows[j].Service.Name })

	s.render(w, "coverage_page", coveragePage{
		Page: "coverage", HasSnapshot: true, Snapshot: snap, Rows: rows,
		BlindSpots: coverage.RankBlindSpots(snap.Grid),
	})
}

// cellFor mirrors internal/coverage.Render's own cell priority exactly
// (matched-certain > global-certain > matched-guess > global-guess), so
// this page never shows a different glyph than `noisefloor coverage`
// prints for the same snapshot.
func cellFor(matches []coverage.RuleMatch) coverageCell {
	if len(matches) == 0 {
		return coverageCell{Glyph: "-", Class: "none", Tooltip: "no rule covers this signal"}
	}
	var matchedCertain, globalCertain, matchedGuess, globalGuess []coverage.RuleMatch
	for _, m := range matches {
		switch {
		case m.Scope == "matched" && m.Certain:
			matchedCertain = append(matchedCertain, m)
		case m.Scope == "global" && m.Certain:
			globalCertain = append(globalCertain, m)
		case m.Scope == "matched":
			matchedGuess = append(matchedGuess, m)
		default:
			globalGuess = append(globalGuess, m)
		}
	}
	pick := func(glyph, class string, ms []coverage.RuleMatch) (coverageCell, bool) {
		if len(ms) == 0 {
			return coverageCell{}, false
		}
		tip := ms[0].GroupName + "/" + ms[0].AlertName + ": " + ms[0].Reason
		return coverageCell{Glyph: glyph, Class: class, Tooltip: tip}, true
	}
	if c, ok := pick("v", "matched-certain", matchedCertain); ok {
		return c
	}
	if c, ok := pick("g", "global-certain", globalCertain); ok {
		return c
	}
	if c, ok := pick("v?", "matched-guess", matchedGuess); ok {
		return c
	}
	if c, ok := pick("g?", "global-guess", globalGuess); ok {
		return c
	}
	return coverageCell{Glyph: "-", Class: "none"} // unreachable
}

// --- JSON API ----------------------------------------------------------

func (s *Server) handleAPIRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rules, err := s.db.ListRules(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	scores, err := s.db.ListLatestScores(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	byRule := make(map[int64]store.Score, len(scores))
	for _, sc := range scores {
		byRule[sc.RuleID] = sc
	}

	out := make([]ruleAPI, 0, len(rules))
	for _, ru := range rules {
		if sc, ok := byRule[ru.ID]; ok {
			out = append(out, newRuleAPI(ru, &sc))
		} else {
			out = append(out, newRuleAPI(ru, nil))
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleAPIRuleDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.badRequest(w, "malformed rule id")
		return
	}
	detail, found, err := s.loadRuleDetail(r.Context(), id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		s.notFound(w, "no such rule")
		return
	}

	limit, offset := episodePaginationParams(r)
	episodes, err := s.db.ListEpisodesForRule(r.Context(), id, limit, offset)
	if err != nil {
		s.serverError(w, err)
		return
	}

	out := ruleDetailAPI{ruleAPI: newRuleAPI(detail.Rule, nil)}
	if detail.HasScore {
		out.Score = newScoreAPI(detail.Score)
	}
	out.EpisodesTotal = detail.EpisodesTotal
	out.EpisodesLimit = limit
	out.EpisodesOffset = offset
	for _, e := range episodes {
		out.Episodes = append(out.Episodes, newEpisodeAPI(e))
	}
	for _, sil := range detail.Silences {
		out.Silences = append(out.Silences, newSilenceAPI(sil))
	}
	if detail.Counterfactual != nil {
		cf := newCounterfactualAPI(detail.P90, *detail.Counterfactual)
		out.Counterfactual = &cf
	}
	writeJSON(w, out)
}

func (s *Server) handleAPIScores(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scores, err := s.db.ListLatestScores(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	rules, err := s.db.ListRules(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	byRule := make(map[int64]store.Rule, len(rules))
	for _, ru := range rules {
		byRule[ru.ID] = ru
	}

	out := make([]scoreRowAPI, 0, len(scores))
	for _, sc := range scores {
		ru := byRule[sc.RuleID]
		out = append(out, scoreRowAPI{
			RuleID: sc.RuleID, AlertName: ru.AlertName, GroupName: ru.GroupName,
			scoreAPI: *newScoreAPI(sc),
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleAPICoverage(w http.ResponseWriter, r *http.Request) {
	snap, ok, err := coverage.Load(r.Context(), s.db)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !ok {
		writeJSON(w, coverageAPI{})
		return
	}
	writeJSON(w, newCoverageAPI(snap))
}

// --- health --------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		http.Error(w, "database unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
