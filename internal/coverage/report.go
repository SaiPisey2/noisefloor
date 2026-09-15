package coverage

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

// Render writes the coverage grid and the ranked blind-spot list to w. This
// is a deliberately separate report from internal/report.Render (`scan`'s
// table): coverage answers a different question, about a different subject
// (services, not rules), from different queries (`up` and a traffic proxy,
// not ALERTS history) -- folding it into the fourteen-column scan table
// would answer neither question well. See cmd/noisefloor's `coverage`
// subcommand.
func Render(w io.Writer, grid []ServiceCoverage, parseErrors map[string]error) error {
	k8s, jobOnly, configOnly := 0, 0, 0
	for _, sc := range grid {
		switch sc.Service.Source {
		case "k8s":
			k8s++
		case "config":
			configOnly++
		default:
			jobOnly++
		}
	}
	fmt.Fprintf(w, "Services   %d discovered (%d via Kubernetes SD labels, %d via job, %d from config only)\n",
		len(grid), k8s, jobOnly, configOnly)

	if len(parseErrors) > 0 {
		fmt.Fprintf(w, "Unparsable %d rule(s) could not be parsed and are excluded from coverage:\n", len(parseErrors))
		keys := make([]string, 0, len(parseErrors))
		for k := range parseErrors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "           %s: %v\n", k, parseErrors[k])
		}
	}

	ambiguous := 0
	for _, sc := range grid {
		for _, sig := range Signals {
			for _, m := range sc.Covered[sig] {
				if !m.Certain {
					ambiguous++
				}
			}
		}
	}
	if ambiguous > 0 {
		fmt.Fprintf(w, "Ambiguous  %d classification(s) below are a guess, marked v? -- rerun with -detail to see why\n", ambiguous)
	}
	fmt.Fprintln(w)

	if len(grid) == 0 {
		fmt.Fprintln(w, "no services discovered")
		return nil
	}

	sorted := append([]ServiceCoverage(nil), grid...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Service.Name < sorted[j].Service.Name })

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tSOURCE\tRATE\tERRORS\tLATENCY\tSATURATION\tBURN_RATE")
	for _, sc := range sorted {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			sc.Service.Name, sc.Service.Source,
			cell(sc, SignalRate), cell(sc, SignalErrors), cell(sc, SignalLatency),
			cell(sc, SignalSaturation), cell(sc, SignalBurnRate))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nv = covered, v? = covered but the classification is a guess, - = no alert covers this signal")

	blind := RankBlindSpots(grid)
	fmt.Fprintln(w, "\nBlind spots, ranked by traffic (idle services are not ranked as urgent):")
	if len(blind) == 0 {
		fmt.Fprintln(w, "  none")
		return nil
	}
	btw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(btw, "SERVICE\tTRAFFIC\tSTATUS")
	for _, b := range blind {
		status := "no alert coverage at all"
		if b.Idle {
			status = "no alert coverage -- idle, not ranked as urgent"
		}
		fmt.Fprintf(btw, "%s\t%.0f\t%s\n", b.Service.Name, b.Service.Traffic, status)
	}
	return btw.Flush()
}

// RenderDetail prints, per service and signal, every rule matched onto it
// and why -- the evidence behind Render's grid, so a "v?" or a surprising
// "-" can be checked rather than trusted blindly. This is the honesty
// mechanism the classification heuristics require: Render alone asserts a
// verdict per cell, RenderDetail shows the reasoning.
func RenderDetail(w io.Writer, grid []ServiceCoverage) error {
	sorted := append([]ServiceCoverage(nil), grid...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Service.Name < sorted[j].Service.Name })

	for _, sc := range sorted {
		fmt.Fprintf(w, "%s (%s)\n", sc.Service.Name, sc.Service.Source)
		any := false
		for _, sig := range Signals {
			matches := sc.Covered[sig]
			if len(matches) == 0 {
				continue
			}
			any = true
			for _, m := range matches {
				certainty := "certain"
				if !m.Certain {
					certainty = "guess"
				}
				gen := ""
				if m.Generator != "" {
					gen = fmt.Sprintf(", generator=%s", m.Generator)
				}
				fmt.Fprintf(w, "  [%s] %s/%s (%s, scope=%s%s): %s\n",
					sig, m.GroupName, m.AlertName, certainty, m.Scope, gen, m.Reason)
			}
		}
		if !any {
			fmt.Fprintln(w, "  no alert coverage on any signal")
		}
	}
	return nil
}

func cell(sc ServiceCoverage, sig Signal) string {
	matches := sc.Covered[sig]
	if len(matches) == 0 {
		return "-"
	}
	for _, m := range matches {
		if m.Certain {
			return "v"
		}
	}
	return "v?"
}
