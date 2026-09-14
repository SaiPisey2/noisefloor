package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

type RuleStore interface {
	UpsertRule(ctx context.Context, r *store.Rule) (int64, error)
	ListRules(ctx context.Context) ([]store.Rule, error)
	MarkRulesInactive(ctx context.Context, keep []int64) error
}

// An interface nothing asserts against is an interface that silently rots.
var _ RuleStore = (*store.SQLite)(nil)

type RuleSyncResult struct {
	Active      int
	Deactivated int

	// DeactivatedRules names the specific rules deactivated this run -- Active
	// before, missing from Prometheus now -- sorted by group then name. A bare
	// count cannot tell an operator "the five rules I deleted on purpose" from
	// "the twelve rules a file that stopped parsing this morning just dropped",
	// which is exactly the distinction that matters.
	DeactivatedRules []DeactivatedRule

	// AmbiguousNames are alert names defined by more than one rule group,
	// sorted. Two groups defining the same name is ordinary in real repos --
	// prod and staging rule files, a mixin vendored twice -- and it is fatal to
	// attribution: the ALERTS series carries an alertname but no group, so
	// every episode of both rules resolves to whichever row the lookup returns
	// first. The other rule then has no episodes and is skipped, and the
	// survivor is scored on a history that is partly someone else's, using its
	// own for: to judge the other rule's episodes.
	//
	// There is no way to disambiguate after the fact; the series genuinely
	// lacks the information. Refusing to score them is the honest answer.
	AmbiguousNames []string
}

// DeactivatedRule identifies one rule SyncRules deactivated because Prometheus
// no longer reports it.
type DeactivatedRule struct {
	GroupName string
	AlertName string
}

// ExprHash identifies a rule's expression. A retuned rule gets a new hash,
// which is how its score starts fresh instead of inheriting the old one.
func ExprHash(expr string) string {
	sum := sha256.Sum256([]byte(expr))
	return hex.EncodeToString(sum[:])[:16]
}

// RetunedDuring reports whether a rule's expression was observed to change
// inside the scoring window. If it was, its stored episodes were produced by an
// expression that no longer exists, and scoring the current rule on them would
// judge it for someone else's behaviour.
//
// A zero ExprChangedAt means the change was never observed -- which is the case
// for every rule on a first scan. Those are NOT retuned: we have simply never
// seen them any other way.
func RetunedDuring(r store.Rule, windowStart time.Time) bool {
	return !r.ExprChangedAt.IsZero() && r.ExprChangedAt.After(windowStart)
}

// SyncRules reconciles the rules Prometheus currently evaluates with what the
// store knows. Rules that disappeared are deactivated, never deleted: their
// history is still evidence, but they must not be scored as live or receive
// pull requests.
//
// maxDeactivatedFraction is config.Rules.MaxDeactivatedFraction: above that
// share of previously active rules disappearing in one run, SyncRules refuses
// rather than deactivating them. See the field's doc comment for why.
func SyncRules(ctx context.Context, groups []prom.RuleGroup, db RuleStore, now time.Time, maxDeactivatedFraction float64) (RuleSyncResult, error) {
	before, err := db.ListRules(ctx)
	if err != nil {
		return RuleSyncResult{}, fmt.Errorf("list existing rules: %w", err)
	}

	var res RuleSyncResult
	keep := make([]int64, 0, len(groups))

	// Count names across every group before upserting anything: a duplicate is
	// a property of the rule set as a whole, not of any one group.
	nameCount := map[string]int{}
	for _, g := range groups {
		for _, r := range g.Alerting {
			nameCount[r.Name]++
		}
	}
	for name, n := range nameCount {
		if n > 1 {
			res.AmbiguousNames = append(res.AmbiguousNames, name)
		}
	}
	sort.Strings(res.AmbiguousNames)

	for _, g := range groups {
		for _, r := range g.Alerting {
			id, err := db.UpsertRule(ctx, &store.Rule{
				AlertName:   r.Name,
				GroupName:   g.Name,
				File:        g.File,
				Expr:        r.Query,
				ExprHash:    ExprHash(r.Query),
				For:         r.For,
				Labels:      r.Labels,
				Annotations: r.Annotations,
				FirstSeen:   now,
				LastSeen:    now,
				// ExprChangedAt is deliberately not set. The store owns that
				// column: zero on insert, and the observation time only when
				// the expression hash actually differs. Passing `now` here
				// would mark every rule in a fresh database as retuned.
				Active: true,
			})
			if err != nil {
				return res, fmt.Errorf("upsert rule %s/%s: %w", g.Name, r.Name, err)
			}
			keep = append(keep, id)
			res.Active++
		}
	}

	kept := make(map[int64]bool, len(keep))
	for _, id := range keep {
		kept[id] = true
	}
	var activeBefore int
	for _, r := range before {
		if !r.Active {
			continue
		}
		activeBefore++
		if !kept[r.ID] {
			res.Deactivated++
			res.DeactivatedRules = append(res.DeactivatedRules,
				DeactivatedRule{GroupName: r.GroupName, AlertName: r.AlertName})
		}
	}
	sort.Slice(res.DeactivatedRules, func(i, j int) bool {
		a, b := res.DeactivatedRules[i], res.DeactivatedRules[j]
		if a.GroupName != b.GroupName {
			return a.GroupName < b.GroupName
		}
		return a.AlertName < b.AlertName
	})

	// Refuse to deactivate everything. Prometheus reporting zero alerting rules
	// when the store already holds active ones is far more likely to be a
	// broken rule file or a reload mid-scrape than a deliberate deletion of
	// every alert an organisation has. Wiping the active flags would make the
	// next scan report nothing at all, silently, and the run after that would
	// have no history to notice the gap. Leave the store alone and say so.
	if len(keep) == 0 && res.Deactivated > 0 {
		return res, fmt.Errorf(
			"prometheus reported no alerting rules but the store holds %d active: "+
				"refusing to deactivate all of them (check the rule files loaded)",
			res.Deactivated)
	}

	// Refuse when a disproportionate share of previously active rules vanish in
	// one run. A handful of rules disappearing is ordinary housekeeping; a rule
	// file that stopped parsing this morning tends to take a much bigger,
	// suddener bite out of the active set, and today the two look identical --
	// a bare count with no names. See MaxDeactivatedFraction's doc comment for
	// why the default sits where it does.
	if activeBefore > 0 {
		if ratio := float64(res.Deactivated) / float64(activeBefore); ratio > maxDeactivatedFraction {
			names := make([]string, len(res.DeactivatedRules))
			for i, d := range res.DeactivatedRules {
				names[i] = d.GroupName + "/" + d.AlertName
			}
			return res, fmt.Errorf(
				"%d of %d previously active rules (%.0f%%) are missing from this run, "+
					"above the configured guard of %.0f%%: refusing to deactivate them "+
					"(this looks like a rule file that stopped parsing, not a deliberate "+
					"cleanup) -- %s",
				res.Deactivated, activeBefore, ratio*100, maxDeactivatedFraction*100,
				strings.Join(names, ", "))
		}
	}

	if err := db.MarkRulesInactive(ctx, keep); err != nil {
		return res, fmt.Errorf("deactivate missing rules: %w", err)
	}
	return res, nil
}
