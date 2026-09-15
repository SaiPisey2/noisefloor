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
//
// Known bias, deliberate and one-directional: tolerating that gap means samples
// at 0 and 2*step merge into one episode of 3*step, where the other reading —
// that the alert genuinely resolved and re-fired — would give two episodes of
// one step each. Every tolerated gap therefore adds one step of phantom firing
// time and removes one episode from the count. Flap rate is scored from episode
// counts, so this biases slightly AGAINST calling a rule flappy. That is the
// safer direction: under-reporting flapping costs a missed tuning suggestion,
// while over-reporting it would propose changes to rules that are fine.
func BuildIntervals(samples []model.SamplePair, step time.Duration) []Interval {
	if len(samples) == 0 || step <= 0 {
		// A non-positive step would make maxGap zero or negative, splitting
		// every sample into its own zero- or negative-length interval and
		// poisoning every duration statistic downstream. config.Validate
		// rejects it at load, but this function is exported and defensive.
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
//
// It assumes one matrix entry per series, which is what a single Prometheus
// range query returns. It does NOT merge entries, so a caller that queries in
// chunks must accumulate across calls and stitch episodes spanning a chunk
// boundary — see the backfiller, which keys an accumulator by
// (alertname, state, fingerprint) and stitches before persisting.
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

// Fingerprint is the exported form of fingerprint (below), for callers
// outside this package that need to compute the identical stable identity
// for a label set. The webhook collector (issue #13) is the reason this
// exists: an episode reported over HTTP must key onto the same
// (alertname, state, fingerprint) identity the backfill computes from
// ALERTS, or the two paths can never recognise the same firing as one
// episode. Callers MUST strip "alertname" (and "alertstate", if present)
// from labels before calling this, exactly as SplitMatrix does, or the
// fingerprints will not agree.
func Fingerprint(labels map[string]string) string { return fingerprint(labels) }

// fingerprint is a stable identity for a LABEL SET, not for an alert. It is
// computed after alertname and alertstate are stripped, so two different alerts
// sharing the same remaining labels produce the same fingerprint.
//
// That is intentional: the fingerprint distinguishes one firing instance of a
// rule from another, and the rule is already identified elsewhere. Callers MUST
// therefore key by (alertname, state, fingerprint), never by fingerprint alone —
// which is exactly what the backfiller's seriesKey and the store's
// UNIQUE (rule_id, fingerprint, started_at, state) both do.
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
