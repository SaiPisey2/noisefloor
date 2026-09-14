// Package scanner holds the scoring loop shared by `noisefloor scan` and
// `noisefloor remediate`: given rules, their episodes and silences already
// fetched, it computes each rule's signals, noise, confidence and verdict
// exactly once. scan renders a subset of the result as a report table;
// remediate reads the same RuleEval to decide which rules it may open a PR
// for and which it must refuse, without re-deriving anything scan already
// worked out.
package scanner

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect"
	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/remediate"
	"github.com/SaiPisey2/noisefloor/internal/report"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// RuleEval is everything known about one rule after a scoring pass:
// unlike report.Row (only active, unambiguous rules with episodes -- what a
// human reads in the scan table), RuleEval covers every rule the store
// knows about, including the ones a remediation proposal must refuse.
type RuleEval struct {
	Rule store.Rule

	// Location is where this rule lives in a git checkout, from
	// remediate.LocateRules. HasLocation is false when rules.path is unset
	// or the rule could not be found under it -- Location is then the zero
	// value and must not be used.
	Location    remediate.RuleLocation
	HasLocation bool

	// HasEpisodes is false for a rule the store knows about but that never
	// fired in the scored window. Signals, Noise, Confidence and Verdict are
	// all zero-value in that case -- there is nothing to score.
	HasEpisodes bool

	Signals    score.Signals
	Noise      float64
	Confidence float64
	Verdict    string

	// WindowStart and WindowEnd are the scan's actual query window
	// (ScoreInput.QueryStart/QueryEnd), copied onto every eval so a
	// consumer building a proposal (internal/pr) has the window without a
	// second lookup. Same value for every RuleEval in one ScoreOutput.
	WindowStart, WindowEnd time.Time

	// Durations is this rule's firing episodes' durations, in the order
	// ListEpisodesInWindow returned them. internal/pr uses it to run
	// remediate.Compute against the exact same distribution
	// Signals.P90Duration was derived from, rather than re-fetching
	// episodes itself.
	Durations []time.Duration

	// SilencedBy is every silence that covered at least one of this rule's
	// firing episodes in the window -- collect.CoveringSilences. A retire
	// proposal's evidence table names these: who silenced the rule, and
	// when.
	SilencedBy []store.Silence

	// SilencesAvailable mirrors report.Meta.SilencesAvailable: whether
	// Alertmanager was actually reachable for this scan. False means
	// Signals.SilencedRate reads 0 and SilencedBy is empty because the data
	// was never fetched, not because the rule was never silenced.
	//
	// Carried per-eval (rather than left in Meta, which only `scan`'s report
	// reads) because a remediation PR body states silenced_rate to a
	// stranger as a measured fact, and has to be able to tell the two cases
	// apart to avoid asserting a number nobody measured.
	SilencesAvailable bool

	// Ambiguous mirrors collect.RuleSyncResult.AmbiguousNames: this alert
	// name is defined in more than one rule group, so its episodes cannot be
	// attributed and it was never scored (Signals/Noise/Confidence/Verdict
	// stay zero-value even if HasEpisodes would otherwise be true).
	Ambiguous bool

	// Retuned mirrors collect.RetunedDuring: this rule's expression changed
	// inside the scored window, so Verdict was forced to score.VerdictKeep
	// and Confidence to 0 regardless of what the signals alone would have
	// said. Kept as its own flag (rather than inferred from Verdict==keep)
	// so a caller can tell "retuned" apart from "genuinely fine" and from
	// "below the confidence floor" -- Verdict collapses all three into the
	// same string.
	Retuned bool
}

// ScoreInput is everything the scoring loop needs, already fetched: no
// network call happens inside Score.
type ScoreInput struct {
	Rules          []store.Rule
	AllEpisodes    []store.Episode // every rule's episodes, for co-fire detection
	Silences       []store.Silence
	AmbiguousNames []string
	// Locations maps (group, alertname) to where a rule lives in a git
	// checkout -- remediate.LocationsByKey's output from LocateRules against
	// config.Rules.Path. Nil-safe: nil or incomplete simply leaves
	// RuleEval.HasLocation false.
	Locations map[remediate.RuleKey]remediate.RuleLocation

	// QueryStart and QueryEnd are the actual backfilled query window
	// (BackfillResult.WindowStart/WindowEnd) -- what gets persisted as each
	// score's window and what RetunedDuring checks a rule's expression
	// change against. NOT necessarily Now-ObservedWindow: QueryEnd is the
	// step-aligned scan boundary, which can differ from Now by up to one
	// step (see cmd/noisefloor's scanWindow).
	QueryStart, QueryEnd time.Time
	// SilencesAvailable is whether Alertmanager was reachable for this scan;
	// it is copied onto every RuleEval. See RuleEval.SilencesAvailable.
	SilencesAvailable bool
	// ObservedWindow is how long this scan actually has evidence for
	// (QueryEnd - QueryStart, or QueryEnd - EarliestData when the backfill
	// was truncated) -- what score.Evaluate measures confidence against.
	ObservedWindow time.Duration
	Now            time.Time
	Cfg            config.Config
}

type ScoreOutput struct {
	// Rows is exactly what `scan` renders: active, unambiguous rules that
	// fired at least once in the window, sorted by report.Render.
	Rows []report.Row
	// Evals covers every rule ScoreInput.Rules named, in the same order.
	Evals []RuleEval
}

// scoreStore is the one write Score needs: persisting the score it just
// computed, same as cmd/noisefloor did directly before this package existed.
// Narrowed to keep Score testable without a real database.
type scoreStore interface {
	UpsertScore(ctx context.Context, sc *store.Score) error
}

var _ scoreStore = (*store.SQLite)(nil)

// Score runs the scoring loop over in.Rules and persists each active,
// unambiguous, fired rule's score via db.UpsertScore -- the same side effect
// cmd/noisefloor's scan has always had. It returns both the report rows scan
// prints and the fuller per-rule evaluation remediate needs.
func Score(ctx context.Context, db scoreStore, in ScoreInput) (ScoreOutput, error) {
	ambiguous := make(map[string]bool, len(in.AmbiguousNames))
	for _, n := range in.AmbiguousNames {
		ambiguous[n] = true
	}

	byRule := map[int64][]store.Episode{}
	for _, e := range in.AllEpisodes {
		byRule[e.RuleID] = append(byRule[e.RuleID], e)
	}

	var out ScoreOutput
	for _, r := range in.Rules {
		eval := RuleEval{
			Rule: r, WindowStart: in.QueryStart, WindowEnd: in.QueryEnd,
			SilencesAvailable: in.SilencesAvailable,
		}
		if loc, ok := in.Locations[remediate.RuleKey{Group: r.GroupName, AlertName: r.AlertName}]; ok {
			eval.Location = loc
			eval.HasLocation = true
		}

		if !r.Active || ambiguous[r.AlertName] {
			eval.Ambiguous = ambiguous[r.AlertName]
			out.Evals = append(out.Evals, eval)
			continue
		}

		eps := byRule[r.ID]
		if len(eps) == 0 {
			out.Evals = append(out.Evals, eval)
			continue
		}
		eval.HasEpisodes = true

		var firing []store.Episode
		for _, e := range eps {
			if e.State == store.StateFiring {
				firing = append(firing, e)
				eval.Durations = append(eval.Durations, e.Duration())
			}
		}
		eval.SilencedBy = collect.CoveringSilences(r.AlertName, firing, in.Silences)

		signals := score.Compute(score.Input{
			Rule: r, Episodes: eps, AllEpisodes: in.AllEpisodes,
			Silences: in.Silences, Location: in.Cfg.Location(),
			FlapWindow: in.Cfg.FlapWindow.Std(),
		})
		noise, confidence, verdict := score.Evaluate(signals, r, in.ObservedWindow, in.Now, in.Cfg)

		if collect.RetunedDuring(r, in.QueryStart) {
			eval.Retuned = true
			verdict = score.VerdictKeep
			confidence = 0
		}

		eval.Signals, eval.Noise, eval.Confidence, eval.Verdict = signals, noise, confidence, verdict

		if err := db.UpsertScore(ctx, &store.Score{
			RuleID: r.ID, WindowStart: in.QueryStart, WindowEnd: in.QueryEnd,
			Signals: signals.Map(), NoiseScore: noise, Verdict: verdict,
			Confidence: confidence, ComputedAt: in.Now,
		}); err != nil {
			return out, fmt.Errorf("upsert score for rule %d (%s/%s): %w", r.ID, r.GroupName, r.AlertName, err)
		}

		out.Rows = append(out.Rows, report.Row{
			AlertName: r.AlertName, GroupName: r.GroupName,
			Verdict: verdict, Noise: noise, Confidence: confidence,
			Signals: signals,
		})
		out.Evals = append(out.Evals, eval)
	}
	return out, nil
}

// ScanWindow returns the query window for a scan started at now: `to`
// rounded DOWN to a multiple of step, and `from` derived from that rounded
// value. See cmd/noisefloor's former scanWindow (this is that function,
// moved here so `remediate` anchors to the same step grid `scan` does --
// see the doc comment on FullResult.QueryEnd for why that anchoring matters
// for deduplication).
func ScanWindow(now time.Time, window, step time.Duration) (from, to time.Time) {
	if step <= 0 {
		return now.Add(-window), now
	}
	to = now.Truncate(step)
	return to.Add(-window), to
}

// FullResult is everything one end-to-end run against Prometheus and
// Alertmanager produces: scores, ready either to render as a report table
// or to feed a remediation proposal.
type FullResult struct {
	ScoreOutput
	Meta report.Meta
}

// RunFull performs one full scan pass -- locate rules in their source
// files, sync rules, backfill episodes, fetch silences, score -- against an
// already-open store and Prometheus client. It is exactly what
// cmd/noisefloor's `scan` has always done end to end; `remediate` calls it
// too, so both commands score a rule identically and neither re-derives
// what the other already computed.
//
// Operational warnings (rule files that failed to parse, rules deactivated
// this run, ambiguous alert names, an unreachable Alertmanager) are printed
// to stderr as they always were -- both commands want the same warnings, so
// printing them once here is not a layering violation, it is the point.
func RunFull(ctx context.Context, cfg config.Config, db *store.SQLite, api prom.Client, now time.Time) (FullResult, error) {
	groups, err := api.Rules(ctx)
	if err != nil {
		return FullResult{}, err
	}

	// Locate each rule in its source file, if a checkout was configured.
	// Failures here are reported and otherwise ignored: rules.path being
	// unset, stale, or pointed at the wrong place must not stop a scan --
	// it only means store.Rule.Line (and RuleEval.HasLocation) stay unset.
	var lines map[remediate.RuleKey]int
	var locations map[remediate.RuleKey]remediate.RuleLocation
	if cfg.Rules.Path != "" {
		locs, fileErrs, lerr := remediate.LocateRules(cfg.Rules.Path)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not read rules.path %s: %v\n", cfg.Rules.Path, lerr)
		} else {
			lines = remediate.LinesByKey(locs)
			locations = remediate.LocationsByKey(locs)
			for _, fe := range fileErrs {
				fmt.Fprintf(os.Stderr, "warning: %s\n", fe)
			}
			for _, f := range remediate.Reconcile(locs, groups) {
				fmt.Fprintf(os.Stderr, "warning: %s\n", f)
			}
		}
	}

	ruleSync, err := collect.SyncRules(ctx, groups, db, now, cfg.Rules.MaxDeactivatedFraction, lines)
	if err != nil {
		return FullResult{}, err
	}

	// A bare deactivation count cannot tell an operator "rules I deleted on
	// purpose" from "a rule file that stopped parsing this morning" -- name
	// them, grouped by rule group, so a real emergency is recognisable at a
	// glance instead of looking like routine cleanup. (Already sorted by
	// group then name.)
	if len(ruleSync.DeactivatedRules) > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d rule(s) deactivated this run (active before, no longer "+
				"reported by prometheus):\n", len(ruleSync.DeactivatedRules))
		var curGroup string
		for _, d := range ruleSync.DeactivatedRules {
			if d.GroupName != curGroup {
				fmt.Fprintf(os.Stderr, "  %s:\n", d.GroupName)
				curGroup = d.GroupName
			}
			fmt.Fprintf(os.Stderr, "    - %s\n", d.AlertName)
		}
	}

	// An alert name defined by two groups cannot be attributed: the ALERTS
	// series carries an alertname and no group, so both rules' episodes land
	// on whichever row the lookup returns, the other rule is silently
	// skipped for having none, and the survivor is judged -- with its own
	// for: -- on a history that is partly someone else's. That produces a
	// confident retire justified by a different rule. Refuse to score them
	// and say so.
	if len(ruleSync.AmbiguousNames) > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d alert name(s) defined in more than one rule group: %s\n"+
				"         the ALERTS series records no group, so their episodes cannot be\n"+
				"         attributed to one rule; they will NOT be scored. Rename them or\n"+
				"         remove the duplicate definition.\n",
			len(ruleSync.AmbiguousNames),
			strings.Join(ruleSync.AmbiguousNames, ", "))
	}

	from, to := ScanWindow(now, cfg.Window.Std(), cfg.Prometheus.Step.Std())
	backfill, err := collect.New(api, db, cfg).Run(ctx, from, to)
	if err != nil {
		return FullResult{}, err
	}

	// silenced_rate carries a quarter of the score, so a failure here is not
	// a detail: every rule scores up to 25 points lower and a `retire` can
	// become a `keep`. Record whether the signal was actually available so
	// the report can say "unavailable" rather than print a 0 that looks like
	// a measurement.
	silencesAvailable := true
	switch {
	case cfg.Alertmanager.URL == "":
		silencesAvailable = false
		fmt.Fprintln(os.Stderr,
			"warning: no alertmanager.url configured; silenced_rate reads 0 for every rule")
	default:
		amRT, rtErr := cfg.Alertmanager.Auth.Transport()
		if rtErr != nil {
			return FullResult{}, fmt.Errorf("alertmanager client: %w", rtErr)
		}
		amClient := &http.Client{Transport: amRT, Timeout: 30 * time.Second}
		fetched, ferr := collect.FetchSilences(ctx, cfg.Alertmanager.URL, amClient)
		if ferr != nil {
			silencesAvailable = false
			fmt.Fprintf(os.Stderr,
				"warning: alertmanager unreachable, silenced_rate reads 0 for every rule: %v\n", ferr)
		} else if err := db.UpsertSilences(ctx, fetched); err != nil {
			return FullResult{}, err
		}
	}

	// Score against everything ever observed, not just this fetch.
	// Alertmanager garbage-collects expired silences (120h by default), so a
	// 30-day window can only ever see the older ones from our own store --
	// which is the whole reason we persist them.
	silences, err := db.ListSilences(ctx, backfill.WindowStart, backfill.WindowEnd)
	if err != nil {
		return FullResult{}, err
	}

	rules, err := db.ListRules(ctx)
	if err != nil {
		return FullResult{}, err
	}

	// Count the store's inactive total, not how many this run deactivated --
	// the run after a rule disappears would otherwise report 0 inactive
	// while the store holds one.
	inactive := 0
	for _, r := range rules {
		if !r.Active {
			inactive++
		}
	}
	allEpisodes, err := db.ListEpisodesInWindow(ctx, backfill.WindowStart, backfill.WindowEnd)
	if err != nil {
		return FullResult{}, err
	}

	// Confidence is earned against time actually observed, not time
	// requested. A fresh Prometheus holding three days of history must not
	// score a rule as though it had been watched for thirty; equally, a full
	// window must not be shortened by a probe's guess. EarliestData is
	// measured from the episodes themselves, so this is exact either way.
	observedWindow := backfill.WindowEnd.Sub(backfill.WindowStart)
	if backfill.Truncated && !backfill.EarliestData.IsZero() {
		observedWindow = backfill.WindowEnd.Sub(backfill.EarliestData)
	}

	scoreOut, err := Score(ctx, db, ScoreInput{
		Rules: rules, AllEpisodes: allEpisodes, Silences: silences,
		AmbiguousNames: ruleSync.AmbiguousNames, Locations: locations,
		QueryStart: backfill.WindowStart, QueryEnd: backfill.WindowEnd,
		SilencesAvailable: silencesAvailable,
		ObservedWindow:    observedWindow, Now: now, Cfg: cfg,
	})
	if err != nil {
		return FullResult{}, err
	}

	return FullResult{
		ScoreOutput: scoreOut,
		Meta: report.Meta{
			WindowStart:       backfill.WindowStart,
			WindowEnd:         backfill.WindowEnd,
			EarliestData:      backfill.EarliestData,
			Truncated:         backfill.Truncated,
			RulesActive:       ruleSync.Active,
			RulesInactive:     inactive,
			Episodes:          backfill.Episodes,
			Silences:          len(silences),
			SilencesAvailable: silencesAvailable,
			Ambiguous:         len(ruleSync.AmbiguousNames),
		},
	}, nil
}
