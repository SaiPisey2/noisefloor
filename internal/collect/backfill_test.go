package collect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

type fakeProm struct {
	floor    time.Time
	noData   bool
	calls    [][2]time.Time
	matrixFn func(start, end time.Time) model.Matrix

	// failAt, when non-zero, makes the failAt'th call (1-based) to QueryRange
	// return queryErr instead of matrixFn's result, so a mid-chunk query
	// failure can be exercised.
	failAt   int
	queryErr error
}

func (f *fakeProm) QueryRange(_ context.Context, _ string, start, end time.Time, _ time.Duration) (model.Matrix, error) {
	f.calls = append(f.calls, [2]time.Time{start, end})
	if f.failAt != 0 && len(f.calls) == f.failAt {
		return nil, f.queryErr
	}
	if f.matrixFn == nil {
		return model.Matrix{}, nil
	}
	return f.matrixFn(start, end), nil
}

func (f *fakeProm) RetentionFloor(_ context.Context, from, to time.Time) (time.Time, error) {
	if f.noData {
		return time.Time{}, prom.ErrEmptyResult
	}
	return f.floor, nil
}

type fakeStore struct {
	rules    map[string]int64
	byName   map[string]int64      // rules the rules collector already created
	created  map[string]store.Rule // every rule UpsertRule has created, by alert name
	inactive []string              // alert names recorded as orphans
	episodes []store.Episode
	nextID   int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rules:   map[string]int64{},
		byName:  map[string]int64{},
		created: map[string]store.Rule{},
		nextID:  1,
	}
}

// withRule pre-registers a rule, standing in for the rules collector having
// run before the backfiller, which is the real order in cmd/noisefloor.
func (s *fakeStore) withRule(alertName string) *fakeStore {
	id := s.nextID
	s.nextID++
	s.byName[alertName] = id
	return s
}

func (s *fakeStore) UpsertRule(_ context.Context, r *store.Rule) (int64, error) {
	key := r.GroupName + "/" + r.AlertName
	if id, ok := s.rules[key]; ok {
		return id, nil
	}
	id := s.nextID
	s.nextID++
	s.rules[key] = id
	// The real store's ON CONFLICT (group_name, alert_name) means a rule is
	// findable by RuleIDByAlertName the instant it is upserted; byName must
	// reflect that so a later lookup for the same alert (a second series
	// fingerprint, or a later chunk) finds this row instead of racing to
	// create a second one.
	s.byName[r.AlertName] = id
	s.created[r.AlertName] = *r
	if !r.Active {
		s.inactive = append(s.inactive, r.AlertName)
	}
	return id, nil
}

func (s *fakeStore) RuleIDByAlertName(_ context.Context, alertName string) (int64, bool, error) {
	if id, ok := s.byName[alertName]; ok {
		return id, true, nil
	}
	return 0, false, nil
}

func (s *fakeStore) InsertEpisodes(_ context.Context, eps []store.Episode) error {
	s.episodes = append(s.episodes, eps...)
	return nil
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.Prometheus.URL = "http://test"
	cfg.Prometheus.Step = config.Duration(time.Minute)
	cfg.Prometheus.Chunk = config.Duration(6 * time.Hour)
	return cfg
}

func TestRunClampsToRetentionFloor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	floor := now.Add(-48 * time.Hour)

	p := &fakeProm{floor: floor}
	b := New(p, newFakeStore(), testConfig())

	res, err := b.Run(context.Background(), now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.WindowStart.Equal(floor) {
		t.Errorf("window start = %v, want retention floor %v", res.WindowStart, floor)
	}
	if !res.Truncated {
		t.Error("Truncated must be true when retention shortens the window")
	}
}

func TestRunTreatsNoHistoryAsAnEmptyWindowNotAnError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	p := &fakeProm{noData: true}
	b := New(p, newFakeStore(), testConfig())

	res, err := b.Run(context.Background(), now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Run returned an error for a Prometheus with no ALERTS history: %v", err)
	}
	if res.Truncated {
		t.Error("Truncated should be false when there is simply no data yet")
	}
	if res.Episodes != 0 {
		t.Errorf("episodes = %d, want 0", res.Episodes)
	}
}

func TestRunChunksTheQuery(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	p := &fakeProm{floor: now.Add(-30 * 24 * time.Hour)}
	b := New(p, newFakeStore(), testConfig())

	// 24h window with a 6h chunk means four queries.
	if _, err := b.Run(context.Background(), now.Add(-24*time.Hour), now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.calls) != 4 {
		t.Fatalf("made %d queries, want 4 (24h / 6h chunk)", len(p.calls))
	}
	for i := 1; i < len(p.calls); i++ {
		if p.calls[i][0].Before(p.calls[i-1][1]) {
			t.Errorf("chunk %d overlaps the previous chunk", i)
		}
	}
}

func TestRunStitchesEpisodesAcrossChunkBoundaries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	start := now.Add(-12 * time.Hour)

	// One alert firing continuously across the whole window, so every chunk
	// sees part of it. It must come out as a single episode.
	p := &fakeProm{
		floor: start.Add(-24 * time.Hour),
		matrixFn: func(cs, ce time.Time) model.Matrix {
			var vals []model.SamplePair
			for t := cs; t.Before(ce); t = t.Add(time.Minute) {
				vals = append(vals, model.SamplePair{
					Timestamp: model.TimeFromUnix(t.Unix()), Value: 1,
				})
			}
			return model.Matrix{{
				Metric: model.Metric{
					"__name__": "ALERTS", "alertname": "Continuous",
					"alertstate": "firing", "instance": "x",
				},
				Values: vals,
			}}
		},
	}
	fs := newFakeStore().withRule("Continuous")
	b := New(p, fs, testConfig())

	res, err := b.Run(context.Background(), start, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.episodes) != 1 {
		t.Fatalf("got %d episodes, want 1 stitched across %d chunks",
			len(fs.episodes), res.Chunks)
	}
	got := fs.episodes[0].Duration()
	if got < 11*time.Hour {
		t.Errorf("stitched duration = %v, want close to 12h", got)
	}
}

func TestRunRecordsStateOnEpisodes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	p := &fakeProm{
		floor: now.Add(-24 * time.Hour),
		matrixFn: func(cs, ce time.Time) model.Matrix {
			return model.Matrix{{
				Metric: model.Metric{
					"__name__": "ALERTS", "alertname": "P",
					"alertstate": "pending", "instance": "x",
				},
				Values: []model.SamplePair{{
					Timestamp: model.TimeFromUnix(cs.Unix()), Value: 1,
				}},
			}}
		},
	}
	fs := newFakeStore().withRule("P")
	b := New(p, fs, testConfig())

	if _, err := b.Run(context.Background(), now.Add(-6*time.Hour), now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.episodes) == 0 {
		t.Fatal("no episodes recorded")
	}
	if fs.episodes[0].State != store.StatePending {
		t.Errorf("state = %q, want %q", fs.episodes[0].State, store.StatePending)
	}
}

func TestRunAttachesEpisodesToExistingRule(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	p := &fakeProm{
		floor: now.Add(-24 * time.Hour),
		matrixFn: func(cs, ce time.Time) model.Matrix {
			return model.Matrix{{
				Metric: model.Metric{
					"__name__": "ALERTS", "alertname": "Known",
					"alertstate": "firing", "instance": "x",
				},
				Values: []model.SamplePair{{
					Timestamp: model.TimeFromUnix(cs.Unix()), Value: 1,
				}},
			}}
		},
	}
	fs := newFakeStore().withRule("Known")
	wantID := fs.byName["Known"]

	if _, err := New(p, fs, testConfig()).Run(context.Background(), now.Add(-6*time.Hour), now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.episodes) == 0 {
		t.Fatal("no episodes recorded")
	}
	if fs.episodes[0].RuleID != wantID {
		t.Errorf("rule id = %d, want %d; backfill must reuse the rule the "+
			"rules collector created, not create a second one",
			fs.episodes[0].RuleID, wantID)
	}
	if len(fs.inactive) != 0 {
		t.Errorf("backfill created %v as orphans; the rule already existed", fs.inactive)
	}
}

func TestRunRecordsUnknownAlertAsInactiveOrphan(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	p := &fakeProm{
		floor: now.Add(-24 * time.Hour),
		matrixFn: func(cs, ce time.Time) model.Matrix {
			return model.Matrix{{
				Metric: model.Metric{
					"__name__": "ALERTS", "alertname": "Deleted",
					"alertstate": "firing", "instance": "x",
				},
				Values: []model.SamplePair{{
					Timestamp: model.TimeFromUnix(cs.Unix()), Value: 1,
				}},
			}}
		},
	}
	fs := newFakeStore() // no rule registered: Prometheus no longer defines it

	if _, err := New(p, fs, testConfig()).Run(context.Background(), now.Add(-6*time.Hour), now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.inactive) != 1 || fs.inactive[0] != "Deleted" {
		t.Errorf("orphans = %v, want [Deleted] recorded as inactive history", fs.inactive)
	}
	// A rule recorded Active: true would be scored, contradicting "never
	// scored" for a series with no live rule. appending to fs.inactive alone
	// does not pin this: assert the field the code actually set.
	if got := fs.created["Deleted"]; got.Active {
		t.Errorf("orphan rule %q recorded Active = true, want false", "Deleted")
	}
}

// TestStitchThresholdMatchesBuildIntervalsGapRule pins the merge threshold
// derived from BuildIntervals' own gap rule. BuildIntervals splits a gap of
// more than 2*step; expressed as a gap between two Intervals (whose End is
// already lastSample+step), that boundary is `step`, not `2*step`. A gap of
// exactly 3*step between raw samples must split into two episodes whether it
// falls mid-chunk (BuildIntervals' job) or on a chunk boundary (stitch's job)
// — the episode count must not depend on where the chunk edge happens to
// land.
func TestStitchThresholdMatchesBuildIntervalsGapRule(t *testing.T) {
	step := time.Minute
	t0 := time.Unix(1_700_000_000, 0).UTC()

	// Chunk 1 covers [t0, t0+10m) and ends with a sample at t0+9m.
	// Chunk 2 covers [t0+10m, t0+20m) and starts with a sample at t0+12m.
	// Raw sample gap = 12m - 9m = 3*step, which BuildIntervals would split.
	lastSample := t0.Add(9 * time.Minute)
	firstSample := t0.Add(12 * time.Minute)

	p := &fakeProm{
		floor: t0.Add(-24 * time.Hour),
		matrixFn: func(cs, ce time.Time) model.Matrix {
			var vals []model.SamplePair
			for _, st := range []time.Time{lastSample, firstSample} {
				if !st.Before(cs) && st.Before(ce) {
					vals = append(vals, model.SamplePair{
						Timestamp: model.TimeFromUnix(st.Unix()), Value: 1,
					})
				}
			}
			if len(vals) == 0 {
				return model.Matrix{}
			}
			return model.Matrix{{
				Metric: model.Metric{
					"__name__": "ALERTS", "alertname": "GapBoundary",
					"alertstate": "firing", "instance": "x",
				},
				Values: vals,
			}}
		},
	}

	cfg := testConfig()
	cfg.Prometheus.Step = config.Duration(step)
	cfg.Prometheus.Chunk = config.Duration(10 * time.Minute)

	fs := newFakeStore().withRule("GapBoundary")
	b := New(p, fs, cfg)

	res, err := b.Run(context.Background(), t0, t0.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Chunks != 2 {
		t.Fatalf("chunks = %d, want 2 (the boundary this test depends on)", res.Chunks)
	}
	if len(fs.episodes) != 2 {
		t.Fatalf("got %d episodes, want 2: a 3*step gap must split, matching "+
			"what BuildIntervals does for the same gap mid-chunk", len(fs.episodes))
	}
}

// TestRunReturnsPartialResultOnMidChunkQueryError exercises the query-error
// path, which a fake that can never fail leaves untested.
func TestRunReturnsPartialResultOnMidChunkQueryError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	wantErr := errors.New("prometheus unavailable")

	// 24h window / 6h chunk = 4 chunks; fail on the second.
	p := &fakeProm{
		floor:    now.Add(-30 * 24 * time.Hour),
		failAt:   2,
		queryErr: wantErr,
	}
	b := New(p, newFakeStore(), testConfig())

	res, err := b.Run(context.Background(), now.Add(-24*time.Hour), now)
	if err == nil {
		t.Fatal("Run: want error from the failing chunk, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want it to wrap %v", err, wantErr)
	}
	if res.Chunks == 0 {
		t.Error("Chunks = 0, want the chunks attempted before the failure")
	}
	if res.WindowStart.IsZero() || res.WindowEnd.IsZero() {
		t.Error("WindowStart/WindowEnd are zero; Run must describe the window " +
			"it was working on even when it fails mid-chunk")
	}
}

// inclusiveMatrixFn mimics real Prometheus query_range semantics: samples at
// both the start and end of the range are included, so two adjacent chunks
// both return the sample that sits exactly on their shared boundary. The
// fakes elsewhere in this file use a half-open [cs, ce) convention instead,
// which never exercises this overlap.
func inclusiveMatrixFn(alertName string, step time.Duration) func(cs, ce time.Time) model.Matrix {
	return func(cs, ce time.Time) model.Matrix {
		var vals []model.SamplePair
		for t := cs; !t.After(ce); t = t.Add(step) {
			vals = append(vals, model.SamplePair{
				Timestamp: model.TimeFromUnix(t.Unix()), Value: 1,
			})
		}
		return model.Matrix{{
			Metric: model.Metric{
				"__name__": "ALERTS", "alertname": model.LabelValue(alertName),
				"alertstate": "firing", "instance": "x",
			},
			Values: vals,
		}}
	}
}

// TestRunHandlesQueryRangeEndpointOverlap checks that a sample duplicated
// across a chunk boundary, as real query_range would return, neither splits
// an episode in two nor inflates its duration. Written against the corrected
// stitch threshold (step, not 2*step).
func TestRunHandlesQueryRangeEndpointOverlap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	start := now.Add(-12 * time.Hour)
	step := time.Minute

	p := &fakeProm{
		floor:    start.Add(-24 * time.Hour),
		matrixFn: inclusiveMatrixFn("Continuous", step),
	}
	fs := newFakeStore().withRule("Continuous")
	b := New(p, fs, testConfig())

	if _, err := b.Run(context.Background(), start, now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.episodes) != 1 {
		t.Fatalf("got %d episodes, want 1; a boundary sample duplicated by "+
			"both adjacent chunks must not split or double-count an episode",
			len(fs.episodes))
	}
	got := fs.episodes[0].Duration()
	// BuildIntervals pads the tail by one step, since a sample at t only
	// means the alert was observed firing at t and stopped somewhere in
	// (t, t+step] — so the true expected duration is the 12h window plus one
	// step, not the window alone. Asserting the exact value, rather than a
	// wide band, is what actually catches inflation from the duplicated
	// boundary sample: a wide band would let a spurious extra step slip
	// through undetected.
	want := 12*time.Hour + step
	if got != want {
		t.Errorf("stitched duration = %v, want exactly %v; a duplicated "+
			"boundary sample must not inflate duration beyond the one-step "+
			"tail padding BuildIntervals always adds", got, want)
	}
}

// TestFakeStoreRuleIDByAlertNameSeesUpsertedRule pins fakeStore's fidelity to
// the real store: RuleIDByAlertName must find a rule immediately after
// UpsertRule creates it, mirroring the real store's
// ON CONFLICT (group_name, alert_name), which makes a second lookup or
// upsert for the same alert idempotent rather than racing to create a
// duplicate row.
func TestFakeStoreRuleIDByAlertNameSeesUpsertedRule(t *testing.T) {
	fs := newFakeStore()
	ctx := context.Background()

	id, err := fs.UpsertRule(ctx, &store.Rule{AlertName: "Deleted", GroupName: ""})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	gotID, found, err := fs.RuleIDByAlertName(ctx, "Deleted")
	if err != nil {
		t.Fatalf("RuleIDByAlertName: %v", err)
	}
	if !found || gotID != id {
		t.Errorf("RuleIDByAlertName after UpsertRule = (%d, %v), want (%d, true)",
			gotID, found, id)
	}
}

func TestRunRejectsNonPositiveChunk(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	from, to := now.Add(-24*time.Hour), now

	for _, bad := range []time.Duration{0, -time.Hour} {
		cfg := testConfig()
		cfg.Prometheus.Chunk = config.Duration(bad)

		p := &fakeProm{floor: now.Add(-30 * 24 * time.Hour)}
		res, err := New(p, newFakeStore(), cfg).Run(context.Background(), from, to)
		if err == nil {
			t.Errorf("chunk %v accepted; the chunk loop would never advance", bad)
		}
		if len(p.calls) != 0 {
			t.Errorf("chunk %v queried Prometheus %d times before failing", bad, len(p.calls))
		}
		if !res.WindowStart.Equal(from) || !res.WindowEnd.Equal(to) {
			t.Errorf("chunk %v returned window %v..%v, want the requested %v..%v; "+
				"Run documents its result as partial but never zero",
				bad, res.WindowStart, res.WindowEnd, from, to)
		}
	}
}
