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

// Cycle periods and base on-durations for the two jittered waveforms
// (spiky, flapping). Kept as named constants, not literals, because
// jitterSeconds and onPeriodFiring below both need them and demo/seed must
// use the identical numbers.
const (
	spikyPeriod    = 5820.0
	spikyBaseOn    = 180.0
	spikyMaxJitter = 180.0 // on-period spreads 3m-6m; off-period never drops below 5460s

	flappingPeriod    = 660.0
	flappingBaseOn    = 240.0
	flappingMaxJitter = 150.0 // on-period spreads 4m-6.5m; off-period never drops below 270s

	// Distinct salts keep the spiky and flapping jitter sequences
	// independent even though both are indexed by an integer cycle count
	// that happens to overlap between the two waveforms.
	spikySalt    = 0x5350494B59 // "SPIKY" read as bytes, just a fixed constant
	flappingSalt = 0x464C415050 // "FLAPP"
)

// splitmix64 is a small, well-distributed, deterministic integer hash. It
// exists here only to turn a cycle index into a uniform pseudo-random
// float -- not for anything resembling cryptography. Must stay byte-for-byte
// identical to demo/seed/main.go's copy: the same cycle index has to produce
// the same jitter in both places for the two waveforms to stay in step.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	x = x ^ (x >> 31)
	return x
}

// jitterSeconds returns a deterministic, right-skewed jitter amount in
// [0, max), seeded from the cycle index (and a per-waveform salt) alone --
// never from wall-clock time (elapsed() is only used to derive the cycle
// index below, not fed into the hash) or any other source that would make a
// re-seed produce different history. The same index always produces the
// same jitter here as in demo/seed, which is what keeps the two waveforms
// in step.
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

// onPeriodFiring reports whether t (seconds since process start) falls
// inside a firing on-period of a cycle-jittered square wave: period-length
// cycles, each with its own on-duration of baseOn+jitterSeconds(cycle
// index). Jitter only ever ADDS to baseOn, so it can never violate the
// >=3-step on-period floor that baseOn alone already satisfies.
func onPeriodFiring(t, period, baseOn float64, salt uint64, maxJitter float64) bool {
	idx := int(t / period)
	on := baseOn + jitterSeconds(idx, salt, maxJitter)
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
	//  4. spiky and flapping additionally carry deterministic jitter on
	//     their on-periods (see jitterSeconds/onPeriodFiring above). A fixed
	//     on-period made every episode's stored duration identical, so a
	//     P90-anchored counterfactual `for:` landed exactly on the
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
	// These periods must stay in step with demo/seed/main.go.

	// Spiky: ~3m-6m firing every 97m (never less than ~94m off). Short-lived,
	// and far enough apart -- past the 1h flap window even at maximum jitter
	// -- that it does not read as flapping. See demo/seed/main.go's matching
	// comment: jitter drops this rule's noise score under the retire
	// threshold on a plain scan with no silences (it was calibrated one
	// point above it); the demo stack's own history and the e2e test's
	// injected silence are what still reach `retire`.
	spiky := 0.0
	if onPeriodFiring(t, spikyPeriod, spikyBaseOn, spikySalt, spikyMaxJitter) {
		spiky = 1
	}

	// Flapping: ~4m-6m on, 5m-7m off. Off-period exceeds the 1m query
	// lookback but stays inside the 1h flap window, so it reads as flapping.
	flapping := 0.0
	if onPeriodFiring(t, flappingPeriod, flappingBaseOn, flappingSalt, flappingMaxJitter) {
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
