package collect

import (
	"context"
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
}

func (f *fakeProm) QueryRange(_ context.Context, _ string, start, end time.Time, _ time.Duration) (model.Matrix, error) {
	f.calls = append(f.calls, [2]time.Time{start, end})
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
	byName   map[string]int64 // rules the rules collector already created
	inactive []string         // alert names recorded as orphans
	episodes []store.Episode
	nextID   int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{rules: map[string]int64{}, byName: map[string]int64{}, nextID: 1}
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
	s.inactive = append(s.inactive, r.AlertName)
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
}
