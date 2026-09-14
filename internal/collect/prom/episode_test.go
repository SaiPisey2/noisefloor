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
	for _, stripped := range []string{"alertstate", "__name__", "alertname"} {
		if a.Labels[stripped] != "" {
			t.Errorf("%s must be stripped from labels, got %v", stripped, a.Labels)
		}
	}
	if a.Labels["instance"] != "x" {
		t.Errorf("instance label lost: %v", a.Labels)
	}
	if byKey["A/firing"].Fingerprint == byKey["B/firing"].Fingerprint {
		t.Error("different label sets must produce different fingerprints")
	}
}

func TestFingerprintIgnoresAlertName(t *testing.T) {
	m := model.Matrix{
		{
			Metric: model.Metric{
				"__name__": "ALERTS", "alertname": "A",
				"alertstate": "firing", "instance": "x",
			},
			Values: samplesAt(0),
		},
		{
			Metric: model.Metric{
				"__name__": "ALERTS", "alertname": "Z",
				"alertstate": "firing", "instance": "x",
			},
			Values: samplesAt(0),
		},
	}

	got := SplitMatrix(m, step)
	if len(got) != 2 {
		t.Fatalf("got %d series groups, want 2", len(got))
	}
	if got[0].Fingerprint != got[1].Fingerprint {
		t.Errorf("alertname must not affect fingerprint: %q vs %q", got[0].Fingerprint, got[1].Fingerprint)
	}
}

func TestFingerprintIsOrderIndependentAndStable(t *testing.T) {
	a := map[string]string{"instance": "x", "job": "api", "severity": "critical"}
	b := map[string]string{"severity": "critical", "instance": "x", "job": "api"}

	fa := fingerprint(a)
	fb := fingerprint(b)
	if fa != fb {
		t.Errorf("fingerprint depends on label order: %q vs %q", fa, fb)
	}

	for i := 0; i < 10; i++ {
		if got := fingerprint(a); got != fa {
			t.Errorf("fingerprint unstable across calls: run %d got %q, want %q", i, got, fa)
		}
	}
}

func TestBuildIntervalsUsesAbsoluteSampleTimes(t *testing.T) {
	base := model.Time(1_700_000_000_000).Time().UTC()

	got := BuildIntervals(samplesAt(0, 1), step)
	if len(got) != 1 {
		t.Fatalf("got %d intervals, want 1", len(got))
	}
	if !got[0].Start.Equal(base) {
		t.Errorf("Start = %v, want %v", got[0].Start, base)
	}
	if want := base.Add(2 * step); !got[0].End.Equal(want) {
		t.Errorf("End = %v, want %v", got[0].End, want)
	}
}

func TestBuildIntervalsSplitsJustPastTolerance(t *testing.T) {
	// model.Time has millisecond resolution, so one millisecond is the
	// smallest gap representable strictly past 2*step; it exercises the same
	// ">" vs ">=" boundary that time.Nanosecond would at finer resolution.
	base := model.Time(1_700_000_000_000)
	justPast := base + model.Time(2*step/time.Millisecond) + 1
	samples := []model.SamplePair{
		{Timestamp: base, Value: 1},
		{Timestamp: justPast, Value: 1},
	}

	got := BuildIntervals(samples, step)
	if len(got) != 2 {
		t.Fatalf("got %d intervals, want 2 (gap just past 2*step must split)", len(got))
	}
}

func TestBuildIntervalsRejectsNonPositiveStep(t *testing.T) {
	if got := BuildIntervals(samplesAt(0, 1, 2), 0); got != nil {
		t.Errorf("got %v, want nil for zero step", got)
	}
	if got := BuildIntervals(samplesAt(0, 1, 2), -time.Minute); got != nil {
		t.Errorf("got %v, want nil for negative step", got)
	}
}
