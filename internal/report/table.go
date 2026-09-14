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
	WindowStart   time.Time
	WindowEnd     time.Time
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
}

func Render(w io.Writer, rows []Row, meta Meta) error {
	days := meta.WindowEnd.Sub(meta.WindowStart).Hours() / 24

	// A truncated window gets minute precision on the start. Printing a whole
	// number of days beside a date-only range reads as a contradiction when
	// retention actually clipped an hour off the front.
	if meta.Truncated {
		fmt.Fprintf(w, "Window     %s to %s  (%.1fd, limited by retention)\n",
			meta.WindowStart.Format("2006-01-02 15:04"),
			meta.WindowEnd.Format("2006-01-02 15:04"), days)
	} else {
		fmt.Fprintf(w, "Window     %s to %s  (%.0fd)\n",
			meta.WindowStart.Format("2006-01-02"),
			meta.WindowEnd.Format("2006-01-02"), days)
	}
	fmt.Fprintf(w, "Rules      %d active, %d inactive\n", meta.RulesActive, meta.RulesInactive)
	fmt.Fprintf(w, "Episodes   %d\n", meta.Episodes)
	if meta.SilencesAvailable {
		fmt.Fprintf(w, "Silences   %d\n\n", meta.Silences)
	} else {
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
	// CONC and NIGHT are here because they drive verdicts: concentration can
	// route a noisy rule to `tune` instead of `retire`, and off-hours carries
	// weight. Without them the table shows a verdict it cannot justify.
	fmt.Fprintln(tw, "NOISE\tCONF\tVERDICT\tGROUP\tRULE\tFIRES\tSHORT\tSILENCED\tFLAP\tCOFIRE\tCONC\tNIGHT")

	for _, r := range sorted {
		fmt.Fprintf(tw, "%.0f\t%.1f\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Noise, r.Confidence, r.Verdict, r.GroupName, r.AlertName,
			r.Signals.Fires,
			pct(r.Signals.ShortLivedRate),
			pct(r.Signals.SilencedRate),
			pct(r.Signals.FlapRate),
			pct(r.Signals.CofireRatio),
			pct(r.Signals.Concentration),
			pct(r.Signals.OffhoursRate),
		)
	}
	return tw.Flush()
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
