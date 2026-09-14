package prom

import (
	"testing"
	"time"

	"github.com/prometheus/common/model"
)

const step = time.Minute

func samplesAt(offsets ...int) []model.SamplePair {
	base := model.Time(1_700_000_000_000) // milliseconds
	out := make([]model.SamplePair, 0, len(offsets))
	for _, o := range offsets {
		out = append(out, model.SamplePair{
			Timestamp: base + model.Time(o*60*1000),
			Value:     1,
		})
	}
	return out
}

func TestBuildIntervalsEmpty(t *testing.T) {
	if got := BuildIntervals(nil, step); got != nil {
		t.Errorf("got %v, want nil for empty input", got)
	}
}

func TestBuildIntervalsSingleSampleLastsOneStep(t *testing.T) {
	got := BuildIntervals(samplesAt(0), step)
	if len(got) != 1 {
		t.Fatalf("got %d intervals, want 1", len(got))
	}
	if got[0].Duration() != step {
		t.Errorf("duration = %v, want %v", got[0].Duration(), step)
	}
}

func TestBuildIntervalsConsecutiveSamplesMerge(t *testing.T) {
	got := BuildIntervals(samplesAt(0, 1, 2, 3, 4), step)
	if len(got) != 1 {
		t.Fatalf("got %d intervals, want 1", len(got))
	}
	if want := 5 * step; got[0].Duration() != want {
		t.Errorf("duration = %v, want %v", got[0].Duration(), want)
	}
}

func TestBuildIntervalsToleratesOneMissedScrape(t *testing.T) {
	// Gap of exactly 2*step: one scrape missed, same episode.
	got := BuildIntervals(samplesAt(0, 1, 3, 4), step)
	if len(got) != 1 {
		t.Fatalf("got %d intervals, want 1 (a 2-step gap must not split)", len(got))
	}
}

func TestBuildIntervalsSplitsOnRealGap(t *testing.T) {
	// Gap of 3*step: genuinely resolved and fired again.
	got := BuildIntervals(samplesAt(0, 1, 4, 5), step)
	if len(got) != 2 {
		t.Fatalf("got %d intervals, want 2", len(got))
	}
	if got[0].Duration() != 2*step {
		t.Errorf("first duration = %v, want %v", got[0].Duration(), 2*step)
	}
	if got[1].Duration() != 2*step {
		t.Errorf("second duration = %v, want %v", got[1].Duration(), 2*step)
	}
	if !got[1].Start.After(got[0].End) {
		t.Error("second interval must start after the first ends")
	}
}

func TestBuildIntervalsSortsUnorderedInput(t *testing.T) {
	got := BuildIntervals(samplesAt(4, 0, 1, 5), step)
	if len(got) != 2 {
		t.Fatalf("got %d intervals, want 2 after sorting", len(got))
	}
	if got[0].Start.After(got[1].Start) {
		t.Error("intervals not ordered by start time")
	}
}

func TestBuildIntervalsDedupes(t *testing.T) {
	got := BuildIntervals(samplesAt(0, 0, 1, 1), step)
	if len(got) != 1 {
		t.Fatalf("got %d intervals, want 1", len(got))
	}
	if want := 2 * step; got[0].Duration() != want {
		t.Errorf("duration = %v, want %v, duplicates must not extend it", got[0].Duration(), want)
	}
}

func TestSplitMatrixSeparatesSeriesAndStates(t *testing.T) {
	m := model.Matrix{
		{
			Metric: model.Metric{
				"__name__": "ALERTS", "alertname": "A",
				"alertstate": "firing", "instance": "x",
			},
			Values: samplesAt(0, 1),
		},
		{
			Metric: model.Metric{
				"__name__": "ALERTS", "alertname": "A",
				"alertstate": "pending", "instance": "x",
			},
			Values: samplesAt(0),
		},
		{
			Metric: model.Metric{
				"__name__": "ALERTS", "alertname": "B",
				"alertstate": "firing", "instance": "y",
			},
			Values: samplesAt(0),
		},
	}

	got := SplitMatrix(m, step)
	if len(got) != 3 {
		t.Fatalf("got %d series groups, want 3", len(got))
	}

	byKey := map[string]SeriesEpisodes{}
	for _, s := range got {
		byKey[s.AlertName+"/"+s.State] = s
	}
	if _, ok := byKey["A/firing"]; !ok {
		t.Error("missing A/firing")
	}
	if _, ok := byKey["A/pending"]; !ok {
		t.Error("missing A/pending")
	}

	a := byKey["A/firing"]
	if a.Labels["alertstate"] != "" || a.Labels["__name__"] != "" {
		t.Errorf("alertstate and __name__ must be stripped from labels, got %v", a.Labels)
	}
	if a.Labels["instance"] != "x" {
		t.Errorf("instance label lost: %v", a.Labels)
	}
	if byKey["A/firing"].Fingerprint == byKey["B/firing"].Fingerprint {
		t.Error("different label sets must produce different fingerprints")
	}
}
