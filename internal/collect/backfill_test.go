package collect

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

type fakeProm struct {
	calls    [][2]time.Time
	matrixFn func(start, end time.Time) model.Matrix

	// failAt, when non-zero, makes the failAt'th call (1-based) to QueryRange
	// return queryErr instead of matrixFn's result, so a mid-chunk query
	// failure can be exercised.
	failAt   int
	queryErr error

	// failTimes, when non-zero, makes the first failTimes calls fail with
	// queryErr and every call after that succeed -- a transient failure that
	// clears up, for exercising retry-then-succeed.
	failTimes int

	// alwaysFail makes every call fail with queryErr -- a failure that never
	// clears up, for exercising exhausted retries and cancellation mid-backoff.
	alwaysFail bool
}

func (f *fakeProm) QueryRange(_ context.Context, _ string, start, end time.Time, _ time.Duration) (model.Matrix, error) {
	f.calls = append(f.calls, [2]time.Time{start, end})
	switch {
	case f.alwaysFail:
		return nil, f.queryErr
	case f.failTimes != 0 && len(f.calls) <= f.failTimes:
		return nil, f.queryErr
	case f.failAt != 0 && len(f.calls) == f.failAt:
		return nil, f.queryErr
	}
	if f.matrixFn == nil {
		return model.Matrix{}, nil
	}
	return f.matrixFn(start, end), nil
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
	// Real retries, but fast: these tests exercise the retry loop itself, not
	// how long it waits.
	cfg.Prometheus.RetryAttempts = 3
	cfg.Prometheus.RetryBaseDelay = config.Duration(5 * time.Millisecond)
	return cfg
}

func TestRunChunksTheQuery(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	p := &fakeProm{}
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
// path, which a fake that can never fail leaves untested. The injected error
// is non-retryable (bad_data, as a 400/422 from Prometheus would be), so it
// must fail the chunk on the first attempt rather than being absorbed by a
// retry.
func TestRunReturnsPartialResultOnMidChunkQueryError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	wantErr := &v1.Error{Type: v1.ErrBadData, Msg: "bad query"}

	// 24h window / 6h chunk = 4 chunks; fail on the second.
	p := &fakeProm{
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
	// Exactly 2 calls: bad_data must not burn any retry attempts.
	if len(p.calls) != 2 {
		t.Errorf("made %d calls, want exactly 2 (chunk 1, then chunk 2's single "+
			"non-retryable attempt); a non-retryable error must fail immediately",
			len(p.calls))
	}
}

// TestRunRetriesTransientFailureThenSucceeds exercises the "fails twice then
// succeeds" path required for issue #4: a transient error must not abort the
// scan when a later attempt would have worked.
func TestRunRetriesTransientFailureThenSucceeds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	p := &fakeProm{
		failTimes: 2,
		queryErr:  errors.New("connection reset by peer"),
		matrixFn: func(cs, ce time.Time) model.Matrix {
			return model.Matrix{{
				Metric: model.Metric{
					"__name__": "ALERTS", "alertname": "Flaky",
					"alertstate": "firing", "instance": "x",
				},
				Values: []model.SamplePair{{
					Timestamp: model.TimeFromUnix(cs.Unix()), Value: 1,
				}},
			}}
		},
	}
	fs := newFakeStore().withRule("Flaky")
	cfg := testConfig()
	cfg.Prometheus.RetryAttempts = 3 // 2 failures + 1 success fits inside 3 attempts

	res, err := New(p, fs, cfg).Run(context.Background(), now.Add(-6*time.Hour), now)
	if err != nil {
		t.Fatalf("Run: want the retry to absorb the transient failure, got: %v", err)
	}
	if len(fs.episodes) == 0 {
		t.Fatal("no episodes recorded; the retried query's data was lost")
	}
	if res.Episodes == 0 {
		t.Error("BackfillResult.Episodes = 0, want the episode from the eventual success")
	}
}

// TestRunExhaustsRetriesAndReturnsPartialResult covers a failure that never
// clears up: attempts must be bounded, and what earlier chunks already wrote
// must survive in the returned, non-zero BackfillResult.
func TestRunExhaustsRetriesAndReturnsPartialResult(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	from := now.Add(-24 * time.Hour) // 4 chunks at the 6h test chunk size

	// failAfterFirstChunk below only ever lets this matrixFn run once (for the
	// first chunk), so it can unconditionally return the "Good" episode.
	p := &fakeProm{
		matrixFn: func(cs, ce time.Time) model.Matrix {
			return model.Matrix{{
				Metric: model.Metric{
					"__name__": "ALERTS", "alertname": "Good",
					"alertstate": "firing", "instance": "x",
				},
				Values: []model.SamplePair{{
					Timestamp: model.TimeFromUnix(cs.Unix()), Value: 1,
				}},
			}}
		},
	}
	fs := newFakeStore().withRule("Good")
	cfg := testConfig()
	cfg.Prometheus.RetryAttempts = 3

	// Fail every call from the second chunk onward, permanently.
	failing := &failAfterFirstChunk{fakeProm: p}
	res, err := New(failing, fs, cfg).Run(context.Background(), from, now)
	if err == nil {
		t.Fatal("Run: want an error once retries are exhausted, got nil")
	}
	if res.Chunks == 0 || res.WindowStart.IsZero() || res.WindowEnd.IsZero() {
		t.Errorf("BackfillResult is effectively zero: %+v", res)
	}
	if len(fs.episodes) == 0 {
		t.Error("earlier, successfully flushed chunks were discarded by the later failure")
	}
	if failing.calls != 1+cfg.Prometheus.RetryAttempts {
		t.Errorf("made %d calls, want exactly %d (1 successful chunk + %d attempts "+
			"on the permanently failing chunk)",
			failing.calls, 1+cfg.Prometheus.RetryAttempts, cfg.Prometheus.RetryAttempts)
	}
}

// failAfterFirstChunk lets the wrapped fakeProm answer its first call
// normally, then fails every call after that with a retryable error -- a
// permanent, transient-looking failure starting on the second chunk.
type failAfterFirstChunk struct {
	*fakeProm
	calls int
}

func (f *failAfterFirstChunk) QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) (model.Matrix, error) {
	f.calls++
	if f.calls == 1 {
		return f.fakeProm.QueryRange(ctx, q, start, end, step)
	}
	return nil, errors.New("connection reset by peer")
}

// TestRunCancelsDuringBackoffPromptly is the cancellation requirement: a
// scan timeout or Ctrl-C during the back-off wait must return promptly, not
// after the full delay elapses.
func TestRunCancelsDuringBackoffPromptly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	p := &fakeProm{alwaysFail: true, queryErr: errors.New("connection reset by peer")}

	cfg := testConfig()
	cfg.Prometheus.RetryAttempts = 5
	// Deliberately long: if cancellation is not honoured promptly, the test
	// either times out or takes multiple seconds instead of well under one.
	cfg.Prometheus.RetryBaseDelay = config.Duration(2 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := New(p, newFakeStore(), cfg).Run(ctx, now.Add(-time.Hour), now)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Run took %v to return after cancellation during back-off, "+
			"want well under the 2s backoff delay", elapsed)
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

		p := &fakeProm{}
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

// sparseMatrixFn returns a single sample at `at`, and nothing anywhere else.
// This is what the deleted retention probe got wrong: sampled hourly with
// instant-query lookback, a lone episode this far into the window made the
// probe report a floor far past the true start of history.
func sparseMatrixFn(alertName string, at time.Time) func(cs, ce time.Time) model.Matrix {
	return func(cs, ce time.Time) model.Matrix {
		if at.Before(cs) || !at.Before(ce) {
			return model.Matrix{}
		}
		return model.Matrix{{
			Metric: model.Metric{
				"__name__": "ALERTS", "alertname": model.LabelValue(alertName),
				"alertstate": "firing", "instance": "x",
			},
			Values: []model.SamplePair{{
				Timestamp: model.TimeFromUnix(at.Unix()), Value: 1,
			}},
		}}
	}
}

// TestRunAlwaysQueriesTheFullRequestedWindow is the core of the retention fix.
// Run must never shorten [from, to): Prometheus returns nothing outside
// retention anyway, so the whole window costs a few empty queries, while
// clipping it silently discarded real episodes.
func TestRunAlwaysQueriesTheFullRequestedWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	from := now.Add(-24 * time.Hour)

	// Data exists only in the very first chunk. A probe would have missed it
	// and clamped the window past it.
	early := from.Add(30 * time.Minute)
	p := &fakeProm{matrixFn: sparseMatrixFn("Sparse", early)}
	fs := newFakeStore().withRule("Sparse")

	res, err := New(p, fs, testConfig()).Run(context.Background(), from, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.WindowStart.Equal(from) || !res.WindowEnd.Equal(now) {
		t.Errorf("window = %v..%v, want the full requested %v..%v",
			res.WindowStart, res.WindowEnd, from, now)
	}
	if p.calls[0][0].After(from) {
		t.Errorf("first chunk started at %v, want %v; the window must never be clipped",
			p.calls[0][0], from)
	}
	if len(fs.episodes) != 1 {
		t.Fatalf("got %d episodes, want 1; the episode in the first chunk must "+
			"not be discarded", len(fs.episodes))
	}
}

// TestRunReportsEarliestEpisodeAsEarliestData pins the replacement for the
// probe: where history begins is measured from the episodes themselves, which
// is exact rather than probabilistic.
func TestRunReportsEarliestEpisodeAsEarliestData(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	from := now.Add(-30 * 24 * time.Hour)
	first := now.Add(-3 * 24 * time.Hour)

	p := &fakeProm{matrixFn: sparseMatrixFn("Late", first)}
	fs := newFakeStore().withRule("Late")

	res, err := New(p, fs, testConfig()).Run(context.Background(), from, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.EarliestData.Equal(first) {
		t.Errorf("EarliestData = %v, want the first episode's start %v", res.EarliestData, first)
	}
	if !res.Truncated {
		t.Error("Truncated must be true when data begins 27 days into the window")
	}
}

// TestRunTreatsDataWithinOneChunkAsNotTruncated is the other side: a rule that
// simply did not fire in the first chunk is not evidence of a retention edge,
// and must not shorten the window the confidence term is measured against.
func TestRunTreatsDataWithinOneChunkAsNotTruncated(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)
	from := now.Add(-30 * 24 * time.Hour)
	first := from.Add(2 * time.Hour) // inside the 6h chunk

	p := &fakeProm{matrixFn: sparseMatrixFn("Early", first)}
	fs := newFakeStore().withRule("Early")

	res, err := New(p, fs, testConfig()).Run(context.Background(), from, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Truncated {
		t.Errorf("Truncated = true for data beginning %v after `from`; one chunk "+
			"of slack must absorb a rule that just did not fire yet",
			first.Sub(from))
	}
}

func TestRunTreatsNoHistoryAsAnEmptyWindowNotAnError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	p := &fakeProm{} // a fresh Prometheus with no ALERTS data at all
	b := New(p, newFakeStore(), testConfig())

	res, err := b.Run(context.Background(), now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Run returned an error for a Prometheus with no ALERTS history: %v", err)
	}
	if res.Truncated {
		t.Error("Truncated should be false when there is simply no data yet")
	}
	if !res.EarliestData.IsZero() {
		t.Errorf("EarliestData = %v, want zero when no episodes exist", res.EarliestData)
	}
	if res.Episodes != 0 {
		t.Errorf("episodes = %d, want 0", res.Episodes)
	}
}
