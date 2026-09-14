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
	// ruleIDs caches the alertname -> rule ID lookup across chunks, so a
	// series that reappears in every chunk of a long scan is looked up (or,
	// for an orphan, upserted) once rather than once per chunk it spans.
	ruleIDs := map[string]int64{}

	for start := res.WindowStart; start.Before(res.WindowEnd); start = start.Add(chunk) {
		end := start.Add(chunk)
		if end.After(res.WindowEnd) {
			end = res.WindowEnd
		}
		res.Chunks++

		m, err := b.queryRangeWithRetry(ctx, `ALERTS`, start, end, step)
		if err != nil {
			// Whatever earlier chunks already settled was flushed to the
			// store as each chunk completed -- see flush below -- so this
			// failure costs only the chunk it happened on, not the scan.
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

		// Persist everything provably settled as of this chunk boundary -- see
		// flush's doc comment for exactly what that means and why it is safe
		// even for a series this chunk never mentioned.
		if err := b.flush(ctx, &res, accum, ruleIDs, step, end, false); err != nil {
			return res, err
		}
	}

	if err := b.flush(ctx, &res, accum, ruleIDs, step, res.WindowEnd, true); err != nil {
		return res, err
	}

	// Exact, not probabilistic: the first episode is the first thing Prometheus
	// could still tell us about. One chunk of slack, because a rule that simply
	// did not fire in the first chunk is not a retention edge.
	res.Truncated = !res.EarliestData.IsZero() && res.EarliestData.Sub(from) > chunk

	return res, nil
}

// flush persists every settled interval in accum and updates res as it goes.
//
// "Settled" means: every interval of a series except possibly its last one,
// which stays held back only while a future chunk could still stitch onto
// it. stitch merges a series' last interval with a later chunk's first one
// when the gap between them is at most one step (see stitch's doc comment),
// and chunks are queried back-to-back, so the earliest a next occurrence of
// a series could possibly start is exactly chunkEnd. That means the last
// interval is ALSO settled, this chunk, the moment
// chunkEnd.Sub(last.End) > step: no sample any later chunk can return would
// be close enough to merge. A series this chunk never mentioned is checked
// against the same rule using its existing last interval, so a rule that
// simply stops firing gets flushed a bare one chunk later, not held until
// the whole window has been scanned. When final is true there is no later
// chunk at all, so everything flushes regardless.
//
// Flushed intervals are removed from accum: a settled interval is written
// once, not re-upserted on every subsequent chunk it is no longer part of.
func (b *Backfiller) flush(ctx context.Context, res *BackfillResult, accum map[seriesKey]*prom.SeriesEpisodes, ruleIDs map[string]int64, step time.Duration, chunkEnd time.Time, final bool) error {
	var toInsert []store.Episode
	now := time.Now().UTC()

	for k, se := range accum {
		n := len(se.Intervals)
		if n == 0 {
			continue
		}
		settled := n - 1
		if final || chunkEnd.Sub(se.Intervals[n-1].End) > step {
			settled = n
		}
		if settled <= 0 {
			continue
		}

		ruleID, ok := ruleIDs[k.alertName]
		if !ok {
			// The ALERTS series carries an alertname but no group. The rules
			// collector has already created every rule Prometheus evaluates,
			// so look the rule up rather than inventing one with an empty
			// group, which would create a second row for a rule that already
			// exists.
			found := false
			var err error
			ruleID, found, err = b.db.RuleIDByAlertName(ctx, k.alertName)
			if err != nil {
				return fmt.Errorf("lookup rule %s: %w", k.alertName, err)
			}
			if !found {
				// No live rule by this name: the series belongs to a rule
				// Prometheus no longer defines. Keep the history, but record
				// it inactive so it is never scored or proposed for change.
				ruleID, err = b.db.UpsertRule(ctx, &store.Rule{
					AlertName: k.alertName,
					GroupName: "",
					FirstSeen: now,
					LastSeen:  now,
					Active:    false,
				})
				if err != nil {
					return fmt.Errorf("record orphaned rule %s: %w", k.alertName, err)
				}
			}
			ruleIDs[k.alertName] = ruleID
		}

		for _, iv := range se.Intervals[:settled] {
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

		if settled == n {
			// Nothing left to hold onto -- including the final-flush case,
			// where settled == n unconditionally.
			delete(accum, k)
		} else {
			se.Intervals = se.Intervals[settled:]
		}
	}

	if err := b.db.InsertEpisodes(ctx, toInsert); err != nil {
		return fmt.Errorf("persist episodes: %w", err)
	}
	res.Episodes += len(toInsert)
	return nil
}

// queryRangeWithRetry wraps Querier.QueryRange with bounded, exponential
// back-off. Only retryable failures -- see prom.IsRetryable -- burn a wait;
// a 400/422 or any other failure that will recur identically fails on the
// first attempt, and a canceled context is never waited out, whether it is
// canceled before the query, during it, or while this is backing off.
func (b *Backfiller) queryRangeWithRetry(ctx context.Context, query string, start, end time.Time, step time.Duration) (model.Matrix, error) {
	attempts := b.cfg.Prometheus.RetryAttempts
	if attempts < 1 {
		attempts = 1
	}
	base := b.cfg.Prometheus.RetryBaseDelay.Std()
	if base <= 0 {
		base = time.Second
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		m, err := b.prom.QueryRange(ctx, query, start, end, step)
		if err == nil {
			return m, nil
		}
		lastErr = err

		if attempt == attempts || !prom.IsRetryable(err) {
			return nil, err
		}

		delay := base << (attempt - 1) // base, 2*base, 4*base, ...
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	// Unreachable: the loop above always returns on its last iteration.
	return nil, lastErr
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
