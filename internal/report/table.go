// Package report renders scan results for a terminal.
package report

import (
	"fmt"
	"io"
	"math"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/score"
)

type Row struct {
	AlertName  string
	GroupName  string
	Verdict    string
	Noise      float64
	Confidence float64
	Signals    score.Signals
}

type Meta struct {
	WindowStart time.Time
	WindowEnd   time.Time

	// EarliestData is where history actually begins: the start of the earliest
	// episode found. Truncated says that is materially later than WindowStart,
	// in which case the window line reports the span of data that exists rather
	// than the span that was asked for.
	EarliestData  time.Time
	Truncated     bool
	RulesActive   int
	RulesInactive int
	Episodes      int
	Silences      int

	// SilencesAvailable is false when Alertmanager could not be reached or was
	// never configured. Every silenced_rate then reads 0, which is a quarter of
	// the score -- the report must say so rather than print "Silences 0", which
	// is indistinguishable from "no silences exist".
	SilencesAvailable bool

	// Ambiguous counts alert names defined in more than one rule group. Their
	// episodes cannot be attributed to a group, so they are not scored at all
	// and do not appear in the table. Their absence has to be stated, or the
	// report silently omits rules the user can see in Prometheus.
	Ambiguous int

	// QueryRetries counts retryable Prometheus query failures absorbed
	// during this scan's backfill (collect.BackfillResult.Retries). Not
	// printed by Render -- a scan that succeeded despite a hiccup is not
	// something a human reads this table to learn -- but carried through
	// so cmd/noisefloor can record it for the server's /metrics
	// (noisefloor_query_failures_total).
	QueryRetries int
}

func Render(w io.Writer, rows []Row, meta Meta) error {
	days := meta.WindowEnd.Sub(meta.WindowStart).Hours() / 24

	// When data begins partway in, say where it begins and how much was asked
	// for -- not "limited by retention", which the scan cannot actually know.
	// A rule set that is simply younger than the window looks identical from
	// here, and claiming retention clipped it would be a guess stated as fact.
	if meta.Truncated && !meta.EarliestData.IsZero() {
		fmt.Fprintf(w, "Window     %s to %s  (%.0fd requested, data begins %s)\n",
			meta.WindowStart.Format("2006-01-02"),
			meta.WindowEnd.Format("2006-01-02"), days,
			meta.EarliestData.Format("2006-01-02"))
	} else {
		fmt.Fprintf(w, "Window     %s to %s  (%.0fd)\n",
			meta.WindowStart.Format("2006-01-02"),
			meta.WindowEnd.Format("2006-01-02"), days)
	}
	fmt.Fprintf(w, "Rules      %d active, %d inactive\n", meta.RulesActive, meta.RulesInactive)
	if meta.Ambiguous > 0 {
		fmt.Fprintf(w, "Ambiguous  %d alert %s defined in more than one group (not scored)\n",
			meta.Ambiguous, plural(meta.Ambiguous, "name", "names"))
	}
	fmt.Fprintf(w, "Episodes   %d\n", meta.Episodes)
	// Three distinct states, and conflating them misreports the score. When
	// Alertmanager is unreachable the store may still hold silences observed by
	// an earlier scan -- which is exactly why they are persisted -- so
	// silenced_rate is NOT necessarily zero, and saying otherwise is false.
	switch {
	case meta.SilencesAvailable:
		fmt.Fprintf(w, "Silences   %d\n\n", meta.Silences)
	case meta.Silences > 0:
		fmt.Fprintf(w, "Silences   %d (from store; alertmanager unavailable, "+
			"newer silences may be missing)\n\n", meta.Silences)
	default:
		fmt.Fprintf(w, "Silences   unavailable (silenced_rate reads 0 for every rule)\n\n")
	}

	if len(rows) == 0 {
		fmt.Fprintln(w, "no rules scored")
		return nil
	}

	// Fully deterministic ordering. Alert names are not unique across groups,
	// and sort.Slice is not stable, so name alone would let two rules swap
	// places between runs of the same scan.
	sorted := append([]Row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		switch {
		case sorted[i].Noise != sorted[j].Noise:
			return sorted[i].Noise > sorted[j].Noise
		case sorted[i].AlertName != sorted[j].AlertName:
			return sorted[i].AlertName < sorted[j].AlertName
		default:
			return sorted[i].GroupName < sorted[j].GroupName
		}
	})

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// CONC, CHURN and NIGHT are here because they drive verdicts: concentration
	// and pending churn can each route a noisy rule to `tune` instead of
	// `retire`, P50 is one of three conditions for `automate`, and off-hours
	// carries scored weight. Without them the table shows a verdict it cannot
	// justify.
	fmt.Fprintln(tw, "NOISE\tCONF\tVERDICT\tGROUP\tRULE\tFIRES\tP50\tSHORT\tSILENCED\tFLAP\tCOFIRE\tCONC\tCHURN\tNIGHT")

	for _, r := range sorted {
		fmt.Fprintf(tw, "%.0f\t%.1f\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Noise, r.Confidence, r.Verdict, r.GroupName, r.AlertName,
			r.Signals.Fires,
			formatDuration(r.Signals.P50Duration),
			pct(r.Signals.ShortLivedRate),
			pct(r.Signals.SilencedRate),
			pct(r.Signals.FlapRate),
			pct(r.Signals.CofireRatio),
			pct(r.Signals.Concentration),
			pct(r.Signals.PendingChurn),
			pct(r.Signals.OffhoursRate),
		)
	}
	return tw.Flush()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// formatDuration renders a duration compactly for the P50 column.
// time.Duration.String() always prints every unit down to the one it started
// at -- 3m0s, 1h2m0s -- which is noise nobody needs at episode-duration
// precision. This drops zero-valued units except where one sits between two
// non-zero units (1h0m5s keeps its zero minutes), and rounds to the second:
// episode durations come from the query step and are never sub-second.
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
	for i, p := range units {
		if p.v != 0 {
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

	var out string
	for i := first; i <= last; i++ {
		out += fmt.Sprintf("%d%s", units[i].v, units[i].u)
	}
	return out
}

// pct renders a rate. Signals are guarded at their source, but this is the
// last thing between a bad float and a user's terminal, so it clamps rather
// than printing "NaN%" or "150%".
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
