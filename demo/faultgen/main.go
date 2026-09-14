// Command faultgen emits metrics engineered to trip a known set of alert
// rules in predictable ways, so scoring can be asserted against rules whose
// intended verdict is known by construction.
package main

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"time"
)

var start = time.Now()

// elapsed returns seconds since process start.
func elapsed() float64 { return time.Since(start).Seconds() }

// Cycle periods for the two jittered waveforms (spiky, flapping). Named
// constants, not literals, because bimodalOn and onPeriodFiring below both
// need them and demo/seed must use the identical numbers.
//
// flappingPeriod is 1800s, not the 660s an unjittered 4m-on/7m-off wave
// would use: its long band (below) needs room to sit comfortably above
// 2x the candidate for: DefaultLongThreshold demands, while its own
// off-period still has to stay inside the 1h flap window. 660s has no room
// for that; 1800s does, with margin either side (see the per-waveform
// comments below).
const (
	spikyPeriod    = 5820.0
	flappingPeriod = 1800.0

	// Distinct salts keep the spiky and flapping cycle draws independent
	// even though both are indexed by an integer cycle count that happens
	// to overlap between the two waveforms.
	spikySalt    = 0x5350494B59 // "SPIKY" read as bytes, just a fixed constant
	flappingSalt = 0x464C415050 // "FLAPP"
)

// shortOn is the on-period (in seconds) both spiky and flapping draw their
// "ordinary" episodes from: 3m or 4m, chosen per cycle. Both values sit
// well under the 5m short-lived floor score/signals.go falls back to
// whenever 3x a rule's for: doesn't reach it -- true for DemoSpiky (no
// for:) and for DemoFlapping (for: 30s, 3x30s=90s < the floor) alike -- so
// every ordinary episode from either rule reads as short-lived regardless
// of which of the two values a given cycle drew.
var shortOn = []float64{180, 240}

// flappingLongOn is flapping's long band: a minority of its episodes run
// genuinely long (15m-17m), which is what gives DemoFlapping's tune
// counterfactual something real to retain -- see flappingLongProb and the
// waveform comment below for the arithmetic this range was chosen against.
var flappingLongOn = []float64{900, 960, 1020}

// flappingLongProb is the fraction of flapping's cycles drawn from
// flappingLongOn rather than shortOn: roughly 1 in 10, the "mostly short,
// occasional long" shape real alert durations take.
const flappingLongProb = 0.10

// splitmix64 is a small, well-distributed, deterministic integer hash. It
// exists here only to turn a cycle index into a uniform pseudo-random
// float -- not for anything resembling cryptography. Must stay byte-for-byte
// identical to demo/seed/main.go's copy: the same cycle index has to
// produce the same draw in both places for the two waveforms to stay in
// step.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	x = x ^ (x >> 31)
	return x
}

// hashUnit returns a deterministic uniform value in [0,1) from a cycle
// index and a salt -- never from wall-clock time (elapsed() is only used to
// derive the cycle index below, not fed into the hash) or any other source
// that would make a re-seed produce different history. The same (index,
// salt) always produces the same value here as in demo/seed, which is what
// keeps the two waveforms in step.
func hashUnit(index int, salt uint64) float64 {
	h := splitmix64(uint64(int64(index))*2 + salt)
	return float64(h%1_000_000) / 1_000_000.0
}

// bimodalOn picks the on-period duration for one cycle: with probability
// longProb (0 disables the long band entirely -- spiky has none) it draws
// uniformly from longValues, otherwise uniformly from shortValues. The
// branch and the in-band pick are separately salted (salt^1 vs salt) so
// they are not the same draw.
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

// onPeriodFiring reports whether t (seconds since process start) falls
// inside a firing on-period of a cycle-bimodal square wave: period-length
// cycles, each with its own on-duration drawn by bimodalOn from the cycle
// index.
func onPeriodFiring(t, period float64, shortValues, longValues []float64, longProb float64, salt uint64) bool {
	idx := int(t / period)
	on := bimodalOn(idx, salt, shortValues, longValues, longProb)
	return math.Mod(t, period) < on
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	t := elapsed()

	// Four constraints bind every period below, and all four are
	// load-bearing:
	//
	//  1. on-period >= 3 sample steps, so the episode survives sampling.
	//  2. off-period > the query lookback (1m, set on the demo Prometheus),
	//     or the gap is invisible and the episodes merge into one.
	//  3. where a rule must read flap_rate 0, off-period > the 1h flap window.
	//  4. spiky and flapping additionally draw a deterministic, bimodal
	//     on-period per cycle (see bimodalOn/onPeriodFiring above) instead
	//     of a single fixed one -- real alerts do not fire for precisely the
	//     same duration every time, and a fixed duration made a P90-anchored
	//     counterfactual `for:` land exactly on the retain/suppress boundary
	//     and suppress nothing. The draw is seeded from the cycle index
	//     alone, so a re-seed reproduces byte-identically.
	//
	// The periods are also deliberately NOT harmonically related. An earlier
	// set had DemoSpiky at 5400s and the cause rules at 600s -- 5400 being an
	// exact multiple of 600, every single Spiky fire coincided with a cause
	// fire and Spiky read cofire_ratio 100%, which is an artefact of the
	// fixture rather than anything about the rule.
	//
	// These periods must stay in step with demo/seed/main.go.

	// Spiky: 3m or 4m firing every 97m (never less than ~94m off). Always
	// short-lived (see shortOn's comment), and far enough apart -- past the
	// 1h flap window -- that it does not read as flapping. No long band:
	// DemoSpiky's noise score is calibrated only one point above the retire
	// threshold (short_lived_rate alone is worth 0.30 of it), so a rule
	// with any real fraction of long episodes falls under the threshold
	// and stops reaching `retire` on self-resolution alone. See
	// internal/e2e/scan_test.go, which seeds a historical silence covering
	// the whole window for exactly this rule and reaches `retire` through
	// silenced_rate as well as short_lived_rate -- that silence is
	// required, not merely a bonus, given how thin this rule's own margin
	// is.
	spiky := 0.0
	if onPeriodFiring(t, spikyPeriod, shortOn, nil, 0, spikySalt) {
		spiky = 1
	}

	// Flapping: 3m or 4m on (90% of cycles) or 15m-17m on (10%), each on
	// followed by an off long enough for the episode to fully resolve
	// first. Off-period exceeds the 1m query lookback but stays inside the
	// 1h flap window in both cases, so it reads as flapping regardless of
	// which band a given cycle drew.
	//
	// The long band exists so DemoFlapping's tune counterfactual has
	// something real to retain: with for: 30s and P90 landing in the short
	// band (~4m, since 90% of episodes sit there), SelectCandidateFor
	// proposes for: ~4m30s and remediate.DefaultLongThreshold sets its
	// "long enough to matter" bar at 2x that, ~9m -- comfortably below this
	// band's 15m floor, so every long episode both survives the candidate
	// for: and counts as long enough to report.
	flapping := 0.0
	if onPeriodFiring(t, flappingPeriod, shortOn, flappingLongOn, flappingLongProb, flappingSalt) {
		flapping = 1
	}

	// Sustained: one long episode every 90m in the live stack, so the demo
	// shows something without waiting a day.
	sustained := 0.0
	if math.Mod(t, 5400) < 1800 {
		sustained = 1
	}

	// Cause A-D: four components that always breach together, which is what
	// cause-based alerting looks like from the outside. Four, not three,
	// because co-fire detection requires three OTHER rules firing alongside.
	// 3m on, 68m off -- past the 1h flap window, so cofire drives the verdict
	// alone instead of flap_rate routing it to tune.
	cause := 0.0
	if math.Mod(t, 4260) < 180 {
		cause = 1
	}

	// Quiet never breaches.
	fmt.Fprintf(w, "# TYPE demo_spiky_gauge gauge\ndemo_spiky_gauge %g\n", spiky)
	fmt.Fprintf(w, "# TYPE demo_flapping_gauge gauge\ndemo_flapping_gauge %g\n", flapping)
	fmt.Fprintf(w, "# TYPE demo_sustained_gauge gauge\ndemo_sustained_gauge %g\n", sustained)
	fmt.Fprintf(w, "# TYPE demo_cause_gauge gauge\n")
	for _, c := range []string{"a", "b", "c", "d"} {
		fmt.Fprintf(w, "demo_cause_gauge{component=%q} %g\n", c, cause)
	}
	fmt.Fprintf(w, "# TYPE demo_quiet_gauge gauge\ndemo_quiet_gauge 0\n")
}

func main() {
	http.HandleFunc("/metrics", metrics)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	log.Println("faultgen listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
