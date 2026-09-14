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

// Cycle periods for the two jittered scenarios (DemoSpiky, DemoFlapping).
// Named constants, not literals, because bimodalOn and onPeriodFiring below
// both need them and demo/faultgen must use the identical numbers.
//
// flappingPeriod is 1800s, not the 660s an unjittered 4m-on/7m-off wave
// would use: its long band (below) needs room to sit comfortably above
// 2x the candidate for: DefaultLongThreshold demands, while its own
// off-period still has to stay inside the 1h flap window. 660s has no room
// for that; 1800s does, with margin either side (see the per-scenario
// comments below).
const (
	spikyPeriod    = 5820.0
	flappingPeriod = 1800.0

	// Distinct salts keep DemoSpiky's and DemoFlapping's cycle draws
	// independent even though both are indexed by an integer cycle count
	// that happens to overlap between the two waveforms.
	spikySalt    = 0x5350494B59 // "SPIKY" read as bytes, just a fixed constant
	flappingSalt = 0x464C415050 // "FLAPP"
)

// shortOn is the on-period (in seconds) both DemoSpiky and DemoFlapping draw
// their "ordinary" episodes from: 3m or 4m, chosen per cycle. Both values
// sit well under the 5m short-lived floor score/signals.go falls back to
// whenever 3x a rule's for: doesn't reach it -- true for DemoSpiky (no
// for:) and for DemoFlapping (for: 30s, 3x30s=90s < the floor) alike -- so
// every ordinary episode from either rule reads as short-lived regardless
// of which of the two values a given cycle drew.
var shortOn = []float64{180, 240}

// flappingLongOn is DemoFlapping's long band: a minority of its episodes
// run genuinely long (15m-17m), which is what gives its tune counterfactual
// something real to retain -- see flappingLongProb and the scenario comment
// below for the arithmetic this range was chosen against.
var flappingLongOn = []float64{900, 960, 1020}

// flappingLongProb is the fraction of DemoFlapping's cycles drawn from
// flappingLongOn rather than shortOn: roughly 1 in 10, the "mostly short,
// occasional long" shape real alert durations take.
const flappingLongProb = 0.10

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

// hashUnit returns a deterministic uniform value in [0,1) from a cycle
// index and a salt -- never from wall-clock time or any other source that
// would make a re-seed produce different history. The same (index, salt)
// always produces the same value, in both demo/seed (writing history) and
// demo/faultgen (emitting it live), which is what keeps the two waveforms
// in step.
func hashUnit(index int, salt uint64) float64 {
	h := splitmix64(uint64(int64(index))*2 + salt)
	return float64(h%1_000_000) / 1_000_000.0
}

// bimodalOn picks the on-period duration for one cycle: with probability
// longProb (0 disables the long band entirely -- DemoSpiky has none) it
// draws uniformly from longValues, otherwise uniformly from shortValues.
// The branch and the in-band pick are separately salted (salt^1 vs salt)
// so they are not the same draw.
//
// Discrete VALUES, not a continuous jittered range quantized after the
// fact: an earlier version of this fixture jittered continuously and let
// the collector's step-aligned sampling quantize the result, which sounds
// equivalent but is not. A continuous range narrower than one sample step
// quantizes to a SINGLE value for nearly every draw (only an exact
// zero-jitter draw lands in the lower bucket), which silently produced one
// fixed duration again -- the exact defect jitter exists to fix, just
// relocated. Choosing from a small, explicit set of values the collector
// reproduces exactly sidesteps that trap: each value's classification
// (short or long, above or below a threshold) is a property of this
// fixture's own design, not an accident of where a continuous range
// happens to straddle a 60-second sampling grid.
func bimodalOn(index int, salt uint64, shortValues, longValues []float64, longProb float64) float64 {
	if longProb > 0 && hashUnit(index, salt^1) < longProb {
		return pickValue(longValues, hashUnit(index, salt))
	}
	return pickValue(shortValues, hashUnit(index, salt))
}

func pickValue(values []float64, u float64) float64 {
	i := int(u * float64(len(values)))
	if i >= len(values) {
		i = len(values) - 1
	}
	return values[i]
}

// onPeriodFiring reports whether t (seconds from window start) falls inside
// a firing on-period of a cycle-bimodal square wave: period-length cycles,
// each with its own on-duration drawn by bimodalOn from the cycle index.
func onPeriodFiring(t, period float64, shortValues, longValues []float64, longProb float64, salt uint64) bool {
	idx := int(t / period)
	on := bimodalOn(idx, salt, shortValues, longValues, longProb)
	return mod(t, period) < on
}

func scenarios() []scenario {
	// Four constraints bind every period here, and all four are load-bearing:
	//
	//  1. on-period >= 3 sample steps, so the episode survives sampling.
	//  2. off-period > the query lookback (1m, set on the demo Prometheus),
	//     or the gap is invisible and the episodes merge into one.
	//  3. where a rule must read flap_rate 0, off-period > the 1h flap window.
	//  4. DemoSpiky and DemoFlapping additionally draw a deterministic,
	//     bimodal on-period per cycle (see bimodalOn/onPeriodFiring above)
	//     instead of a single fixed one -- real alerts do not fire for
	//     precisely the same duration every time, and a fixed duration made
	//     a P90-anchored counterfactual `for:` land exactly on the
	//     retain/suppress boundary and suppress nothing. The draw is seeded
	//     from the cycle index alone, so a re-seed reproduces
	//     byte-identically.
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
			// 3m or 4m firing every 97m (never less than ~94m off): always
			// short-lived (see shortOn's comment), and far enough apart --
			// past the 1h flap window -- that it does not register as
			// flapping. No long band: DemoSpiky's noise score is calibrated
			// only one point above the retire threshold (short_lived_rate
			// alone is worth 0.30 of it), so a rule with any real fraction
			// of long episodes falls under the threshold and stops reaching
			// `retire` on self-resolution alone. See
			// internal/e2e/scan_test.go, which seeds a historical silence
			// covering the whole window for exactly this rule and reaches
			// `retire` through silenced_rate as well as short_lived_rate --
			// that silence is required, not merely a bonus, given how thin
			// this rule's own margin is.
			alertname: "DemoSpiky", severity: "warning",
			firing: func(t float64) bool {
				return onPeriodFiring(t, spikyPeriod, shortOn, nil, 0, spikySalt)
			},
		},
		{
			// 3m or 4m on (90% of cycles) or 15m-17m on (10%), each on
			// followed by an off long enough for the episode to fully
			// resolve first. Off-period exceeds the 1m query lookback but
			// stays inside the 1h flap window in both cases, so it reads as
			// flapping regardless of which band a given cycle drew.
			//
			// The long band exists so DemoFlapping's tune counterfactual has
			// something real to retain: with for: 30s and P90 landing in the
			// short band (~4m, since 90% of episodes sit there),
			// SelectCandidateFor proposes for: ~4m30s and
			// remediate.DefaultLongThreshold sets its "long enough to
			// matter" bar at 2x that, ~9m -- comfortably below this band's
			// 15m floor, so every long episode both survives the candidate
			// for: and counts as long enough to report. See
			// internal/pr's regenerated tune body for the resulting
			// sentence.
			alertname: "DemoFlapping", severity: "warning",
			firing: func(t float64) bool {
				return onPeriodFiring(t, flappingPeriod, shortOn, flappingLongOn, flappingLongProb, flappingSalt)
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
