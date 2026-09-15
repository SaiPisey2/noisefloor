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
func Render(w io.Writer, result Result) error {
	grid, parseErrors := result.Grid, result.ParseErrors

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

	if len(result.ExcludedJobs) > 0 {
		sorted := append([]string(nil), result.ExcludedJobs...)
		sort.Strings(sorted)
		fmt.Fprintf(w, "Excluded   job(s) %s dropped via coverage.exclude_jobs -- not shown below; "+
			"set exclude_jobs: [] to include them\n", sorted)
	}

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

	// An unattributable rule is not a coverage gap, and the difference
	// matters more here than anywhere else in this report: every row below
	// reads "-" for both. Saying how many rules landed on nobody lets a
	// reader tell an inflated blind-spot list from a real one, instead of
	// being handed the two as if they were the same finding.
	if len(result.Unattributed) > 0 {
		fmt.Fprintf(w, "Unattributed %d rule(s) could not be attributed to any service: they parsed and "+
			"classified, but name no service discovered here (a rule scoped to an exporter job, or to a "+
			"service this Prometheus does not scrape). They are NOT counted as coverage for anyone, so a "+
			"\"-\" below may be one of these rather than a gap:\n", len(result.Unattributed))
		for _, k := range result.Unattributed {
			fmt.Fprintf(w, "             %s\n", k)
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
	fmt.Fprintln(w, "\nBlind spots -- services no rule names specifically (a cluster-wide rule covering")
	fmt.Fprintln(w, "every target counts in the grid above, but is not alerting on THIS service). Ranked")
	fmt.Fprintln(w, "by traffic within each basis (req/s ranked above scrape-sample counts -- the two are")
	fmt.Fprintln(w, "not comparable; idle services are not ranked as urgent):")
	if len(blind) == 0 {
		fmt.Fprintln(w, "  none")
		return nil
	}
	btw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(btw, "SERVICE\tTRAFFIC\tBASIS\tSTATUS")
	for _, b := range blind {
		status := "no rule names this service"
		if b.Idle {
			status = "no rule names this service -- idle, not ranked as urgent"
		}
		basis := b.Service.TrafficBasis
		if basis == "" {
			basis = "n/a"
		}
		fmt.Fprintf(btw, "%s\t%s\t%s\t%s\n", b.Service.Name, formatTraffic(b.Service), basis, status)
	}
	return btw.Flush()
}

// formatTraffic renders a traffic value at a precision appropriate to its
// basis: request rates are small, fractional numbers worth two decimal
// places; sample-volume counts are integers.
func formatTraffic(s Service) string {
	if s.TrafficBasis == BasisRequests {
		return fmt.Sprintf("%.2f", s.Traffic)
	}
	return fmt.Sprintf("%.0f", s.Traffic)
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
