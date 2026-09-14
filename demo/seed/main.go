// Command seed writes an OpenMetrics file containing synthetic ALERTS
// history, which promtool converts into TSDB blocks. This gives the demo
// stack scoreable history immediately instead of after several days.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// scenario describes one alert rule's synthetic firing behaviour.
type scenario struct {
	alertname string
	severity  string
	labels    map[string]string
	// firing reports whether the alert is firing at offset t from the window
	// start, in seconds.
	firing func(t float64) bool
}

func scenarios() []scenario {
	// Every on-period below is at least three sample steps long, and every
	// off-period is longer than the collector's two-step gap tolerance.
	// This is not cosmetic: a two-step on-period has zero margin, because
	// Prometheus' evaluation grid runs on a phase independent of the query
	// grid and can shave a step off either edge, aliasing an episode away
	// entirely. Three steps is the floor. Keep these in step with
	// demo/faultgen.
	return []scenario{
		{
			// 3m firing every 90m: short-lived, and far enough apart that it
			// does not register as flapping. Reaches `retire` only once a
			// silence exists against it, which is the correct bar.
			alertname: "DemoSpiky", severity: "warning",
			firing: func(t float64) bool { return mod(t, 5400) < 180 },
		},
		{
			// 4m on, 4m off: short episodes that keep coming back.
			alertname: "DemoFlapping", severity: "warning",
			firing: func(t float64) bool { return mod(t, 480) < 240 },
		},
		{
			// One long episode a day: a real alert.
			alertname: "DemoSustained", severity: "page",
			firing: func(t float64) bool { return mod(t, 86400) < 3600 },
		},
		// Four rules, not three: co-fire detection requires three OTHER
		// rules firing alongside, so three would leave each one short.
		{
			alertname: "DemoCauseA", severity: "warning",
			firing: func(t float64) bool { return mod(t, 600) < 180 },
		},
		{
			alertname: "DemoCauseB", severity: "warning",
			firing: func(t float64) bool { return mod(t, 600) < 180 },
		},
		{
			alertname: "DemoCauseC", severity: "warning",
			firing: func(t float64) bool { return mod(t, 600) < 180 },
		},
		{
			alertname: "DemoCauseD", severity: "warning",
			firing: func(t float64) bool { return mod(t, 600) < 180 },
		},
	}
}

func mod(a, b float64) float64 {
	m := a - b*float64(int(a/b))
	if m < 0 {
		m += b
	}
	return m
}

// Generate writes OpenMetrics text for the ALERTS series across [start, end)
// at the given step.
func Generate(w io.Writer, start, end time.Time, step time.Duration) error {
	if _, err := fmt.Fprint(w, "# TYPE ALERTS gauge\n"); err != nil {
		return err
	}
	stepSec := step.Seconds()
	total := end.Sub(start).Seconds()

	for _, sc := range scenarios() {
		for t := 0.0; t < total; t += stepSec {
			if !sc.firing(t) {
				continue
			}
			ts := start.Add(time.Duration(t) * time.Second).Unix()
			line := fmt.Sprintf(
				`ALERTS{alertname=%q,alertstate="firing",severity=%q,instance="faultgen:8080",job="faultgen"} 1 %d`,
				sc.alertname, sc.severity, ts)
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprint(w, "# EOF\n")
	return err
}

// splitSample splits an OpenMetrics sample line into its series identity and
// timestamp. Used by tests to assert per-series ordering.
func splitSample(line string) (series string, ts float64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return "", 0, false
	}
	return fields[0], v, true
}

func main() {
	var (
		out   = flag.String("out", "alerts.openmetrics", "output file")
		days  = flag.Int("days", 30, "days of history to generate")
		stepS = flag.Duration("step", time.Minute, "sample step")
	)
	flag.Parse()

	end := time.Now().UTC().Truncate(time.Hour)
	start := end.AddDate(0, 0, -*days)

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create %s: %v", *out, err)
	}
	defer f.Close()

	if err := Generate(f, start, end, *stepS); err != nil {
		log.Fatalf("generate: %v", err)
	}
	log.Printf("wrote %s covering %s to %s", *out, start.Format(time.RFC3339), end.Format(time.RFC3339))
}
