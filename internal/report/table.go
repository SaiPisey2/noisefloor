// Package report renders scan results for a terminal.
package report

import (
	"fmt"
	"io"
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
}

func Render(w io.Writer, rows []Row, meta Meta) error {
	days := meta.WindowEnd.Sub(meta.WindowStart).Hours() / 24
	note := ""
	if meta.Truncated {
		note = "  (limited by retention)"
	}

	fmt.Fprintf(w, "Window     %s to %s  (%.0fd)%s\n",
		meta.WindowStart.Format("2006-01-02"),
		meta.WindowEnd.Format("2006-01-02"), days, note)
	fmt.Fprintf(w, "Rules      %d active, %d inactive\n", meta.RulesActive, meta.RulesInactive)
	fmt.Fprintf(w, "Episodes   %d\n", meta.Episodes)
	fmt.Fprintf(w, "Silences   %d\n\n", meta.Silences)

	if len(rows) == 0 {
		fmt.Fprintln(w, "no rules scored")
		return nil
	}

	sorted := append([]Row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Noise != sorted[j].Noise {
			return sorted[i].Noise > sorted[j].Noise
		}
		return sorted[i].AlertName < sorted[j].AlertName
	})

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NOISE\tCONF\tVERDICT\tRULE\tFIRES\tSHORT\tSILENCED\tFLAP\tCOFIRE")

	for _, r := range sorted {
		fmt.Fprintf(tw, "%.0f\t%.1f\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			r.Noise, r.Confidence, r.Verdict, r.AlertName,
			r.Signals.Fires,
			pct(r.Signals.ShortLivedRate),
			pct(r.Signals.SilencedRate),
			pct(r.Signals.FlapRate),
			pct(r.Signals.CofireRatio),
		)
	}
	return tw.Flush()
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }
