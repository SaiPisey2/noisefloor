package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
func SyncRules(ctx context.Context, groups []prom.RuleGroup, db RuleStore, now time.Time) (RuleSyncResult, error) {
	before, err := db.ListRules(ctx)
	if err != nil {
		return RuleSyncResult{}, fmt.Errorf("list existing rules: %w", err)
	}

	var res RuleSyncResult
	keep := make([]int64, 0, len(groups))

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
	for _, r := range before {
		if !kept[r.ID] && r.Active {
			res.Deactivated++
		}
	}

	if err := db.MarkRulesInactive(ctx, keep); err != nil {
		return res, fmt.Errorf("deactivate missing rules: %w", err)
	}
	return res, nil
}
