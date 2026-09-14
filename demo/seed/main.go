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
	"sort"
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

// Cycle periods and base on-durations for the two jittered scenarios
// (DemoSpiky, DemoFlapping). Kept as named constants, not literals, because
// jitterSeconds and onPeriodFiring below both need them and demo/faultgen
// must use the identical numbers.
const (
	spikyPeriod    = 5820.0
	spikyBaseOn    = 180.0
	spikyMaxJitter = 180.0 // on-period spreads 3m-6m; off-period never drops below 5460s

	flappingPeriod    = 660.0
	flappingBaseOn    = 240.0
	flappingMaxJitter = 150.0 // on-period spreads 4m-6.5m; off-period never drops below 270s

	// Distinct salts keep DemoSpiky's and DemoFlapping's jitter sequences
	// independent even though both are indexed by an integer cycle count
	// that happens to overlap between the two waveforms.
	spikySalt    = 0x5350494B59 // "SPIKY" read as bytes, just a fixed constant
	flappingSalt = 0x464C415050 // "FLAPP"
)

// splitmix64 is a small, well-distributed, deterministic integer hash. It
// exists here only to turn a cycle index into a uniform pseudo-random
// float -- not for anything resembling cryptography.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	x = x ^ (x >> 31)
	return x
}

// jitterSeconds returns a deterministic, right-skewed jitter amount in
// [0, max), seeded from the cycle index (and a per-waveform salt) alone --
// never from wall-clock time or any other source that would make a re-seed
// produce different history. The same index always produces the same
// jitter, in both demo/seed (writing history) and demo/faultgen (emitting
// it live), which is what keeps the two waveforms in step.
//
// u^3 with u uniform on [0,1) is the skew: most cycles land near the short
// end (median jitter is ~1/8 of max) with a tail reaching toward the full
// max, which is the shape real alert durations take -- mostly short,
// occasionally much longer -- rather than the single fixed duration this
// waveform used to produce every cycle.
func jitterSeconds(index int, salt uint64, max float64) float64 {
	h := splitmix64(uint64(int64(index))*2 + salt)
	u := float64(h%1_000_000) / 1_000_000.0
	return u * u * u * max
}

// onPeriodFiring reports whether t (seconds from window start) falls inside
// a firing on-period of a cycle-jittered square wave: period-length cycles,
// each with its own on-duration of baseOn+jitterSeconds(cycle index).
// Jitter only ever ADDS to baseOn, so it can never violate the >=3-step
// on-period floor that baseOn alone already satisfies.
func onPeriodFiring(t, period, baseOn float64, salt uint64, maxJitter float64) bool {
	idx := int(t / period)
	on := baseOn + jitterSeconds(idx, salt, maxJitter)
	return mod(t, period) < on
}

func scenarios() []scenario {
	// Four constraints bind every period here, and all four are load-bearing:
	//
	//  1. on-period >= 3 sample steps, so the episode survives sampling.
	//  2. off-period > the query lookback (1m, set on the demo Prometheus),
	//     or the gap is invisible and the episodes merge into one.
	//  3. where a rule must read flap_rate 0, off-period > the 1h flap window.
	//  4. DemoSpiky and DemoFlapping additionally carry deterministic jitter
	//     on their on-periods (see jitterSeconds/onPeriodFiring above). A
	//     fixed on-period made every episode's stored duration identical, so
	//     a P90-anchored counterfactual `for:` landed exactly on the
	//     retain/suppress boundary and suppressed nothing -- real alerts do
	//     not fire for precisely the same duration every time. Jitter is
	//     seeded from the cycle index, not wall-clock time or any RNG state,
	//     so a re-seed reproduces byte-identically. Both jitter ranges stay
	//     well inside constraints 2 and 3 even at maximum jitter (see the
	//     per-constant comments above).
	//
	// The periods are also deliberately NOT harmonically related. An earlier
	// set had DemoSpiky at 5400s and the cause rules at 600s -- 5400 being an
	// exact multiple of 600, every single Spiky fire coincided with a cause
	// fire and Spiky read cofire_ratio 100%, which is an artefact of the
	// fixture rather than anything about the rule.
	//
	// Every on-period below is at least three sample steps long, and every
	// off-period is longer than the collector's two-step gap tolerance.
	// This is not cosmetic: a 20s pulse sampled every 60s aliases away
	// entirely, and a 45s-on/45s-off wave sampled at 60s produces gaps of at
	// most two steps, which the collector merges into one continuous episode
	// with a flap rate of zero. Keep these in step with demo/faultgen.
	return []scenario{
		{
			// ~3m-6m firing every 97m (never less than ~94m off): short-lived,
			// and far enough apart -- past the 1h flap window even at maximum
			// jitter -- that it does not register as flapping.
			//
			// Before jitter, every episode was exactly 3m and short_lived_rate
			// alone (worth 0.30 of noise) put this rule's noise score at 31,
			// one point above the retire threshold of 30 -- calibrated with no
			// margin to spare. Jitter now pushes roughly a third of episodes to
			// 5m/6m (still short-lived by any human standard, but over the
			// fixed 5m short_lived floor score/signals.go uses when a rule has
			// no `for:`), which drops short_lived_rate enough to fall under the
			// threshold: DemoSpiky verdicts `keep` on a plain scan with no
			// silences. See internal/e2e/scan_test.go, which seeds a
			// historical silence covering the whole window for exactly this
			// rule and reaches `retire` through silenced_rate instead -- that
			// margin was always doing real work, jitter just made it visible.
			alertname: "DemoSpiky", severity: "warning",
			firing: func(t float64) bool {
				return onPeriodFiring(t, spikyPeriod, spikyBaseOn, spikySalt, spikyMaxJitter)
			},
		},
		{
			// ~4m-6m on, 5m-7m off: short episodes that keep coming back.
			// Off-period exceeds the 1m query lookback but stays inside the
			// 1h flap window, so it reads as flapping.
			alertname: "DemoFlapping", severity: "warning",
			firing: func(t float64) bool {
				return onPeriodFiring(t, flappingPeriod, flappingBaseOn, flappingSalt, flappingMaxJitter)
			},
		},
		{
			// One long episode a day: a real alert.
			alertname: "DemoSustained", severity: "page",
			firing: func(t float64) bool { return mod(t, 90000) < 3600 },
		},
		// Four rules, not three: co-fire detection requires three OTHER
		// rules firing alongside, so three would leave each one short.
		// 3m on, 68m off -- past the 1h flap window, so cofire drives the
		// verdict alone instead of flap_rate routing it to tune.
		{
			alertname: "DemoCauseA", severity: "warning",
			labels: map[string]string{"component": "a"},
			firing: func(t float64) bool { return mod(t, 4260) < 180 },
		},
		{
			alertname: "DemoCauseB", severity: "warning",
			labels: map[string]string{"component": "b"},
			firing: func(t float64) bool { return mod(t, 4260) < 180 },
		},
		{
			alertname: "DemoCauseC", severity: "warning",
			labels: map[string]string{"component": "c"},
			firing: func(t float64) bool { return mod(t, 4260) < 180 },
		},
		{
			alertname: "DemoCauseD", severity: "warning",
			labels: map[string]string{"component": "d"},
			firing: func(t float64) bool { return mod(t, 4260) < 180 },
		},
	}
}

// sortedKeys keeps label order deterministic, so regenerating the seed file
// produces byte-identical output for the same inputs.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
			// Extra labels must match what the live rule produces. A seeded
			// episode that lacks a label the live rule adds becomes a second
			// fingerprint for the same rule, and a 600-to-1 split across two
			// fingerprints scores a concentration near 1.0 -- which routes the
			// rule to `tune` on the strength of a fixture artefact.
			extra := ""
			for _, k := range sortedKeys(sc.labels) {
				extra += fmt.Sprintf(",%s=%q", k, sc.labels[k])
			}
			line := fmt.Sprintf(
				`ALERTS{alertname=%q,alertstate="firing",severity=%q,instance="faultgen:8080",job="faultgen"%s} 1 %d`,
				sc.alertname, sc.severity, extra, ts)
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
