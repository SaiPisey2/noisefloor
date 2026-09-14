package prom

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	"github.com/prometheus/common/model"
)

// Interval is one continuous firing (or pending) period of a single series.
type Interval struct {
	Start time.Time
	End   time.Time
}

func (i Interval) Duration() time.Duration { return i.End.Sub(i.Start) }

// BuildIntervals reconstructs firing intervals from the sample timestamps of
// one ALERTS series.
//
// A sample at t means the alert was observed firing at t; it stopped somewhere
// in (t, t+step]. Episodes therefore end at lastSample+step, which
// overestimates by at most one step, consistently. Callers record the step as
// the episode's resolution so no downstream consumer reports finer precision
// than the data supports.
//
// A gap larger than 2*step closes an episode. Exactly 2*step is tolerated, so
// a single missed scrape does not split one episode into two.
func BuildIntervals(samples []model.SamplePair, step time.Duration) []Interval {
	if len(samples) == 0 {
		return nil
	}

	times := make([]time.Time, 0, len(samples))
	for _, s := range samples {
		times = append(times, s.Timestamp.Time().UTC())
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	// Drop duplicates so repeated timestamps cannot inflate a duration.
	deduped := times[:1]
	for _, t := range times[1:] {
		if !t.Equal(deduped[len(deduped)-1]) {
			deduped = append(deduped, t)
		}
	}

	maxGap := 2 * step
	var out []Interval
	start, last := deduped[0], deduped[0]

	for _, t := range deduped[1:] {
		if t.Sub(last) > maxGap {
			out = append(out, Interval{Start: start, End: last.Add(step)})
			start = t
		}
		last = t
	}
	out = append(out, Interval{Start: start, End: last.Add(step)})
	return out
}

// SeriesEpisodes groups the reconstructed intervals of one ALERTS series.
type SeriesEpisodes struct {
	AlertName   string
	State       string
	Fingerprint string
	Labels      map[string]string
	Intervals   []Interval
}

// SplitMatrix turns an ALERTS matrix into per-series episodes. The synthetic
// labels __name__, alertname and alertstate are stripped from Labels, since
// they identify the series rather than describe the alert instance.
func SplitMatrix(m model.Matrix, step time.Duration) []SeriesEpisodes {
	out := make([]SeriesEpisodes, 0, len(m))
	for _, series := range m {
		labels := map[string]string{}
		for k, v := range series.Metric {
			switch string(k) {
			case "__name__", "alertname", "alertstate":
				continue
			}
			labels[string(k)] = string(v)
		}
		out = append(out, SeriesEpisodes{
			AlertName:   string(series.Metric["alertname"]),
			State:       string(series.Metric["alertstate"]),
			Fingerprint: fingerprint(labels),
			Labels:      labels,
			Intervals:   BuildIntervals(series.Values, step),
		})
	}
	return out
}

// fingerprint is a stable identity for a label set, used to tell one firing
// instance of a rule from another across scans.
func fingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(labels[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
