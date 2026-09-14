// Package collect turns Prometheus and Alertmanager history into stored
// episodes and silences.
package collect

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/common/model"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// Querier is the slice of the Prometheus API the backfiller needs. It is
// deliberately narrower than prom.Client so tests can fake it cheaply.
type Querier interface {
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (model.Matrix, error)
}

// Store is the slice of the store the backfiller writes to.
type Store interface {
	RuleIDByAlertName(ctx context.Context, alertName string) (int64, bool, error)
	UpsertRule(ctx context.Context, r *store.Rule) (int64, error)
	InsertEpisodes(ctx context.Context, eps []store.Episode) error
}

// An interface nothing asserts against is an interface that silently rots.
// The same omission let prom.Client drift out of sync with *prom.API.
var (
	_ Querier = (*prom.API)(nil)
	_ Store   = (*store.SQLite)(nil)
)

type BackfillResult struct {
	// WindowStart and WindowEnd are always the window that was REQUESTED. The
	// backfiller never shortens it: Prometheus returns nothing outside
	// retention anyway, so querying the whole of it costs nothing and cannot
	// discard data.
	WindowStart time.Time
	WindowEnd   time.Time

	// EarliestData is the start of the earliest episode actually reconstructed,
	// or zero if there were none. It is an exact measurement of where history
	// begins, unlike probing `count(ALERTS)`: a probe evaluates each point with
	// instant-query lookback, so for sparse alerting it reports a floor far
	// later than the first real sample.
	EarliestData time.Time

	// Episodes counts the episodes reconstructed in the window, NOT rows
	// written. InsertEpisodes upserts, so re-running over an overlapping
	// window reports the same count having written nothing new.
	Episodes int
	Chunks   int

	// Truncated says the window holds less history than was asked for: data
	// begins more than one chunk after WindowStart. One chunk of slack, because
	// a sparse rule may simply not have fired in the first chunk -- that is not
	// evidence of a retention edge.
	Truncated bool
}

type Backfiller struct {
	prom Querier
	db   Store
	cfg  config.Config
}

func New(p Querier, db Store, cfg config.Config) *Backfiller {
	return &Backfiller{prom: p, db: db, cfg: cfg}
}

// seriesKey identifies one ALERTS series across chunk boundaries.
type seriesKey struct {
	alertName   string
	state       string
	fingerprint string
}

// Run backfills episodes for the whole of [from, to).
//
// It deliberately does NOT clamp the window to a probed retention floor.
// Prometheus answers with nothing outside retention, so the extra chunks cost
// a few empty queries; clamping, by contrast, cost real data. The probe used
// `count(ALERTS)` sampled hourly, and Prometheus evaluates each point with
// instant-query lookback, so a rule that fires rarely produced a floor far
// later than its first episode. Every episode before that floor was then never
// queried, and the shortened window dragged the confidence term down until
// every rule came back `keep`.
//
// Where history actually begins is reported instead as EarliestData, measured
// from the episodes themselves.
//
// PRECONDITION: the rules collector must have run first. Run attaches episodes
// to rules by alert name; if it runs first, a currently-defined alert gets an
// orphan row under an empty group, the collector then inserts the real rule
// under its real group, and every episode stays attached to the inactive
// orphan and is never scored.
//
// On error the returned BackfillResult is partial but never zero: it describes
// what had been done when the error occurred.
func (b *Backfiller) Run(ctx context.Context, from, to time.Time) (BackfillResult, error) {
	if b.cfg.Prometheus.Chunk.Std() <= 0 {
		// A non-positive chunk makes the loop below never advance.
		// config.Validate rejects it, but New accepts an unvalidated Config.
		//
		// Returns the requested window rather than a zero value, per the
		// partial-but-never-zero contract above: nothing has been done yet,
		// so the window is the only thing worth reporting.
		return BackfillResult{WindowStart: from, WindowEnd: to},
			fmt.Errorf("prometheus.chunk must be positive, got %v", b.cfg.Prometheus.Chunk)
	}

	res := BackfillResult{WindowStart: from, WindowEnd: to}

	step := b.cfg.Prometheus.Step.Std()
	chunk := b.cfg.Prometheus.Chunk.Std()

	// Accumulate per-series intervals across chunks, then stitch, so an
	// episode spanning a boundary is not reported as two.
	accum := map[seriesKey]*prom.SeriesEpisodes{}

	for start := res.WindowStart; start.Before(res.WindowEnd); start = start.Add(chunk) {
		end := start.Add(chunk)
		if end.After(res.WindowEnd) {
			end = res.WindowEnd
		}
		res.Chunks++

		m, err := b.prom.QueryRange(ctx, `ALERTS`, start, end, step)
		if err != nil {
			return res, fmt.Errorf("backfill chunk %s..%s: %w",
				start.Format(time.RFC3339), end.Format(time.RFC3339), err)
		}

		for _, se := range prom.SplitMatrix(m, step) {
			k := seriesKey{se.AlertName, se.State, se.Fingerprint}
			existing, ok := accum[k]
			if !ok {
				cp := se
				accum[k] = &cp
				continue
			}
			existing.Intervals = stitch(existing.Intervals, se.Intervals, step)
		}
	}

	var toInsert []store.Episode
	now := time.Now().UTC()

	for k, se := range accum {
		// The ALERTS series carries an alertname but no group. The rules
		// collector has already created every rule Prometheus evaluates, so
		// look the rule up rather than inventing one with an empty group,
		// which would create a second row for a rule that already exists.
		ruleID, found, err := b.db.RuleIDByAlertName(ctx, k.alertName)
		if err != nil {
			return res, fmt.Errorf("lookup rule %s: %w", k.alertName, err)
		}
		if !found {
			// No live rule by this name: the series belongs to a rule
			// Prometheus no longer defines. Keep the history, but record it
			// inactive so it is never scored or proposed for change.
			ruleID, err = b.db.UpsertRule(ctx, &store.Rule{
				AlertName: k.alertName,
				GroupName: "",
				FirstSeen: now,
				LastSeen:  now,
				Active:    false,
			})
			if err != nil {
				return res, fmt.Errorf("record orphaned rule %s: %w", k.alertName, err)
			}
		}
		for _, iv := range se.Intervals {
			if res.EarliestData.IsZero() || iv.Start.Before(res.EarliestData) {
				res.EarliestData = iv.Start
			}
			toInsert = append(toInsert, store.Episode{
				RuleID:      ruleID,
				Fingerprint: se.Fingerprint,
				// Every episode of this series shares one map. Read-only
				// downstream today, but mutating it would corrupt them all.
				Labels:     se.Labels,
				StartedAt:  iv.Start,
				EndedAt:    iv.End,
				Resolution: step,
				Source:     store.SourceBackfill,
				State:      se.State,
			})
		}
	}

	// Exact, not probabilistic: the first episode is the first thing Prometheus
	// could still tell us about. One chunk of slack, because a rule that simply
	// did not fire in the first chunk is not a retention edge.
	res.Truncated = !res.EarliestData.IsZero() && res.EarliestData.Sub(from) > chunk

	if err := b.db.InsertEpisodes(ctx, toInsert); err != nil {
		return res, fmt.Errorf("persist episodes: %w", err)
	}
	res.Episodes = len(toInsert)
	return res, nil
}

// stitch appends next to prev, merging the boundary pair when they are close
// enough to be the same episode split by a chunk edge.
//
// The threshold is `step`, not `2*step`, and the difference is load-bearing.
// BuildIntervals merges two samples when their separation is <= 2*step. Here
// the comparison is against last.End, which is already lastSample+step, so
// comparing to 2*step would merge samples up to 3*step apart. A gap of exactly
// 3*step would then split mid-chunk but merge on a chunk boundary, making an
// episode's duration and the episode count depend on chunk alignment. Flap
// rate is scored from episode counts, so that would make scores depend on a
// configuration value that has nothing to do with the alert.
//
//	merge when firstSample - lastSample <= 2*step
//	firstSample = first.Start, lastSample = last.End - step
//	=> first.Start - last.End <= step
func stitch(prev, next []prom.Interval, step time.Duration) []prom.Interval {
	if len(prev) == 0 {
		return next
	}
	if len(next) == 0 {
		return prev
	}
	last := prev[len(prev)-1]
	first := next[0]

	if first.Start.Sub(last.End) <= step {
		prev[len(prev)-1] = prom.Interval{Start: last.Start, End: first.End}
		return append(prev, next[1:]...)
	}
	return append(prev, next...)
}
