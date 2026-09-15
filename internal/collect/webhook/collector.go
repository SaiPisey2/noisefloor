package webhook

import (
	"context"
	"fmt"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// Store is the slice of the store the collector writes to. Deliberately
// narrower than *store.SQLite so tests can fake it cheaply -- the same
// pattern internal/collect.Store follows for the backfiller.
type Store interface {
	RuleIDByAlertName(ctx context.Context, alertName string) (int64, bool, error)
	UpsertRule(ctx context.Context, r *store.Rule) (int64, error)
	UpsertWebhookEpisode(ctx context.Context, ep store.Episode, meta store.WebhookMeta) (id int64, reconciled bool, err error)
}

var _ Store = (*store.SQLite)(nil)

// Collector turns validated webhook payloads into stored episodes.
type Collector struct {
	db      Store
	nowFunc func() time.Time
}

func New(db Store) *Collector {
	return &Collector{db: db, nowFunc: time.Now}
}

// Result summarizes one Ingest call, returned to the caller (and, from the
// HTTP handler, to Alertmanager -- which ignores the body but this makes
// the endpoint useful to curl by hand too).
type Result struct {
	AlertsProcessed int `json:"alertsProcessed"`
	EpisodesNew     int `json:"episodesNew"`
	EpisodesMerged  int `json:"episodesMerged"`
}

// Ingest records every alert in p as an episode, reconciled against
// whatever this series already has stored. Alerts within one payload that
// fail individually (an empty alertname slipping past Validate, say) are
// skipped rather than failing the whole notification -- Alertmanager does
// not retry on 2xx, so one bad alert must not cost the rest of the group
// the episodes they legitimately reported.
func (c *Collector) Ingest(ctx context.Context, p Payload) (Result, error) {
	var res Result
	now := c.nowFunc().UTC()

	for _, a := range p.Alerts {
		alertName := a.Labels["alertname"]
		if alertName == "" {
			continue // Validate should already have rejected this; defensive.
		}

		ruleID, err := c.resolveRuleID(ctx, alertName, now)
		if err != nil {
			return res, fmt.Errorf("resolve rule %q: %w", alertName, err)
		}

		labels := make(map[string]string, len(a.Labels))
		for k, v := range a.Labels {
			if k == "alertname" || k == "alertstate" {
				continue
			}
			labels[k] = v
		}
		fp := prom.Fingerprint(labels)

		start := a.StartsAt.UTC()
		end := a.EndsAt.UTC()
		switch a.Status {
		case statusResolved:
			// EndsAt is authoritative for a resolved alert.
		default:
			// Still firing: Alertmanager's EndsAt during firing is an
			// auto-resolve estimate (startsAt + resolve_timeout), not a
			// measurement, so it is not trustworthy as an episode boundary.
			// The best available estimate of "still firing as of" is the
			// instant this notification was processed; it is extended by
			// every repeat notification and, eventually, corrected exactly
			// by the resolved notification via the exact-match path in
			// UpsertWebhookEpisode.
			end = now
		}
		if !end.After(start) {
			end = start.Add(time.Second)
		}

		ep := store.Episode{
			RuleID:      ruleID,
			Fingerprint: fp,
			Labels:      labels,
			StartedAt:   start,
			EndedAt:     end,
			Resolution:  0, // webhook timing is exact, not step-bounded
			Source:      store.SourceWebhook,
			State:       store.StateFiring, // webhook never reports "pending"
		}
		meta := store.WebhookMeta{
			Receiver:         p.Receiver,
			GroupKey:         p.GroupKey,
			GroupLabels:      p.GroupLabels,
			Annotations:      a.Annotations,
			GeneratorURL:     a.GeneratorURL,
			ExternalURL:      p.ExternalURL,
			PreciseStartedAt: start,
			PreciseEndedAt:   end,
			UpdatedAt:        now,
		}

		_, reconciled, err := c.db.UpsertWebhookEpisode(ctx, ep, meta)
		if err != nil {
			return res, fmt.Errorf("upsert episode for %s: %w", alertName, err)
		}

		res.AlertsProcessed++
		if reconciled {
			res.EpisodesMerged++
		} else {
			res.EpisodesNew++
		}
	}

	return res, nil
}

// resolveRuleID finds the rule this alert belongs to by name, the same way
// the backfiller does (see collect.Backfiller.flush), creating a
// placeholder rule when the rules collector has not synced this alert name
// yet. Unlike the backfiller's placeholder, this one is recorded ACTIVE --
// see below for why.
//
// That differs from the backfiller's own orphan handling, which records an
// unmatched ALERTS series as an inactive rule: for the backfill, a series
// with no matching current rule almost always means the rule has since
// been deleted, because the rules collector runs first (see the
// precondition on Backfiller.Run) and would otherwise have already created
// a row for it. Nothing analogous holds here: a webhook notification IS a
// currently-firing alert, so a rule Prometheus has not synced yet is far
// more likely to mean `noisefloor scan` simply has not run since this
// collector started, not that the rule was deleted. Marking it inactive
// would then withhold real evidence from scoring until the next scan
// happened to also see it.
//
// Once the rules collector does sync this alert name under its real
// group, RuleIDByAlertName's `ORDER BY active DESC` means every subsequent
// call here (and from the backfiller) resolves to that real, active row;
// only episodes recorded before that first sync stay attached to this
// placeholder. Same accepted imprecision the backfiller already documents
// for its own orphans.
func (c *Collector) resolveRuleID(ctx context.Context, alertName string, now time.Time) (int64, error) {
	id, ok, err := c.db.RuleIDByAlertName(ctx, alertName)
	if err != nil {
		return 0, fmt.Errorf("lookup rule: %w", err)
	}
	if ok {
		return id, nil
	}
	id, err = c.db.UpsertRule(ctx, &store.Rule{
		AlertName: alertName,
		GroupName: "",
		FirstSeen: now,
		LastSeen:  now,
		Active:    true,
	})
	if err != nil {
		return 0, fmt.Errorf("create placeholder rule: %w", err)
	}
	return id, nil
}
