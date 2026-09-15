package pr

import (
	"context"
	"fmt"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/remediate"
)

// resolveTarget decides which existing rule group a starter rule for sc's
// service should be appended to, or returns the zero Target when none can
// be determined.
//
// Two ways to find one, tried in this order, and never a third:
//
//  1. A group that already covers this exact service on some OTHER signal
//     (a "matched"-scope RuleMatch -- see internal/coverage.MapRules), found
//     automatically by looking up that rule's own file via
//     remediate.LocationsByKey. This is the natural home for a new rule
//     about a service: right next to the rules a team already wrote for
//     it. In practice this rarely fires for a genuine blind spot (see
//     starter.go's package doc comment: a service with literally no
//     coverage on any signal usually has no group mentioning it at all
//     either) -- it is kept for when propose is pointed at a
//     partially-covered service, and is independently unit-tested via
//     BuildStarter/resolveTarget directly.
//  2. An explicit nomination in config.Coverage.RuleTargets, keyed by
//     service name -- verified to actually exist (that file, that group)
//     in the current checkout before being trusted, so a stale or
//     mistyped nomination surfaces as ReasonNoTarget rather than a broken
//     edit.
//
// Never a third option: resolveTarget will not invent a new file or a new
// group. See config.Coverage.RuleTargets's doc comment for why.
func resolveTarget(sc coverage.ServiceCoverage, cfg config.Config, locByKey map[remediate.RuleKey]remediate.RuleLocation) Target {
	for _, sig := range coverage.Signals {
		for _, m := range sc.Covered[sig] {
			if m.Scope != "matched" {
				continue
			}
			loc, ok := locByKey[remediate.RuleKey{Group: m.GroupName, AlertName: m.AlertName}]
			if !ok {
				continue
			}
			return Target{File: loc.File, Group: m.GroupName}
		}
	}

	if t, ok := cfg.Coverage.RuleTargets[sc.Service.Name]; ok {
		for _, loc := range locByKey {
			if loc.File == t.File && loc.Key.Group == t.Group {
				return Target{File: t.File, Group: t.Group}
			}
		}
	}
	return Target{}
}

// matchedOnly returns only the matches in ms whose Scope is "matched",
// dropping global-scope matches before they ever reach BuildStarter.
//
// N2: BuildStarter's existing-coverage check (see StarterInput.
// ExistingMatches) treats ANY certain match as "genuinely covered, nothing
// to propose" -- correct for a rule that names this service, but not for
// one that sweeps up every service in the cluster. Before this filter,
// sc.Covered[sig] (which MapRules populates with both scopes) was passed
// through unfiltered, so a cluster-wide certain rule such as `up == 0`
// silently suppressed the rate starter for every blind-spot service in the
// fleet -- no proposal, no refusal, the signal simply absent from the
// output. Filtering to matched-scope here keeps BuildStarter's contract
// ("existing match means covered") true only for the kind of match
// RankBlindSpots itself already treats as covering a service
// (AnyScopedCoverage), rather than reintroducing global attribution through
// the one path that was not yet checking for it.
func matchedOnly(ms []coverage.RuleMatch) []coverage.RuleMatch {
	var out []coverage.RuleMatch
	for _, m := range ms {
		if m.Scope == "matched" {
			out = append(out, m)
		}
	}
	return out
}

// RunStarters is coverage's counterpart to Run: it proposes at most one
// starter-rule pull request per blind-spot service found by
// coverage.RankBlindSpots (see StarterPriority for which signal), reusing
// openProposal for everything about actually opening the PR -- idempotence,
// dry run, -apply -- exactly as Run does for retire/tune.
//
// now is passed in (rather than read from time.Now inside) so a test can
// probe against a fixed instant the way scanner.RunFull already takes one.
func RunStarters(ctx context.Context, provider Provider, api prom.Client, cfg config.Config, result coverage.Result, now time.Time, opt RunOptions) (RunResult, error) {
	var res RunResult

	locs, _, err := remediate.LocateRules(cfg.Rules.Path)
	if err != nil {
		return res, fmt.Errorf("locate rules: %w", err)
	}
	locByKey := remediate.LocationsByKey(locs)

	byName := make(map[string]coverage.ServiceCoverage, len(result.Grid))
	for _, sc := range result.Grid {
		byName[sc.Service.Name] = sc
	}

	for _, bs := range coverage.RankBlindSpots(result.Grid) {
		if bs.Idle {
			res.Refusals = append(res.Refusals, Refusal{Group: bs.Service.Name, AlertName: "-", Reason: ReasonIdle})
			continue
		}
		sc := byName[bs.Service.Name]

		for _, sig := range StarterPriority {
			m, err := probe(ctx, api, sc.Service, sig, cfg.Window.Std(), now)
			if err != nil {
				return res, fmt.Errorf("%s: probe %s: %w", sc.Service.Name, sig, err)
			}
			in := StarterInput{
				Service: sc.Service, Signal: sig,
				// Filtered to matched-scope only, consistent with
				// RankBlindSpots'/AnyScopedCoverage's own definition of
				// "covered" -- see StarterInput.ExistingMatches's doc
				// comment for why a global match must not reach
				// BuildStarter here.
				ExistingMatches: matchedOnly(sc.Covered[sig]),
				Measurement:     m,
				Target:          resolveTarget(sc, cfg, locByKey),
			}
			proposal, refusal, err := BuildStarter(in)
			if err != nil {
				return res, fmt.Errorf("%s/%s: %w", sc.Service.Name, sig, err)
			}
			if refusal != nil {
				res.Refusals = append(res.Refusals, *refusal)
				continue
			}
			if proposal == nil {
				continue // genuinely covered already; try nothing further for this signal
			}

			if err := proposal.Render(); err != nil {
				return res, fmt.Errorf("%s/%s: render edit: %w", sc.Service.Name, sig, err)
			}
			res.Proposals = append(res.Proposals, proposal)
			if err := openProposal(ctx, provider, proposal, opt, &res); err != nil {
				return res, err
			}
			break // one starter rule per blind-spot service per run -- see StarterPriority
		}
	}

	return res, nil
}
