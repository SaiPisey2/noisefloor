package server

import (
	"fmt"
	"math"
	"time"
)

// formatDuration renders a duration compactly (3m, 45s, 1h2m), the same
// shape internal/report and internal/remediate use for the same numbers --
// a rule's P50/P90 duration should read identically on the terminal and on
// this page.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)

	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	units := [3]struct {
		v int64
		u string
	}{{int64(h), "h"}, {int64(m), "m"}, {int64(s), "s"}}

	first := -1
	for i, u := range units {
		if u.v != 0 {
			first = i
			break
		}
	}
	if first == -1 {
		return "0s"
	}
	last := len(units) - 1
	for last > first && units[last].v == 0 {
		last--
	}

	out := ""
	for i := first; i <= last; i++ {
		out += fmt.Sprintf("%d%s", units[i].v, units[i].u)
	}
	return out
}

// pct renders a rate as a percentage, clamped the same way
// internal/report's pct is: a bad float becomes "-" or a clamped bound
// rather than "NaN%" or "150%" reaching a page.
func pct(f float64) string {
	switch {
	case math.IsNaN(f):
		return "-"
	case f < 0:
		f = 0
	case f > 1:
		f = 1
	}
	return fmt.Sprintf("%.0f%%", f*100)
}

// pctOf reads key from a signals map (as stored on store.Score, e.g.
// "short_lived_rate") and renders it with pct. Missing keys render as "-":
// a rule scored before a signal existed, or a map that failed to
// deserialise, must not silently print 0%, which looks like a measurement.
func pctOf(signals map[string]float64, key string) string {
	v, ok := signals[key]
	if !ok {
		return "-"
	}
	return pct(v)
}

func durationOf(signals map[string]float64, key string) time.Duration {
	return time.Duration(signals[key] * float64(time.Second))
}

func intOf(signals map[string]float64, key string) int {
	return int(signals[key])
}
