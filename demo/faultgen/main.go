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

func metrics(w http.ResponseWriter, _ *http.Request) {
	t := elapsed()

	// Three constraints bind every period below, and all three are
	// load-bearing:
	//
	//  1. on-period >= 3 sample steps, so the episode survives sampling.
	//  2. off-period > the query lookback (1m, set on the demo Prometheus),
	//     or the gap is invisible and the episodes merge into one.
	//  3. where a rule must read flap_rate 0, off-period > the 1h flap window.
	//
	// The periods are also deliberately NOT harmonically related. An earlier
	// set had DemoSpiky at 5400s and the cause rules at 600s -- 5400 being an
	// exact multiple of 600, every single Spiky fire coincided with a cause
	// fire and Spiky read cofire_ratio 100%, which is an artefact of the
	// fixture rather than anything about the rule.
	//
	// These periods must stay in step with demo/seed/main.go.

	// Spiky: 3m firing every 97m (94m off). Short-lived, and far enough apart
	// -- past the 1h flap window -- that it does not read as flapping.
	spiky := 0.0
	if math.Mod(t, 5820) < 180 {
		spiky = 1
	}

	// Flapping: 4m on, 7m off. Off-period exceeds the 1m query lookback but
	// stays inside the 1h flap window, so it reads as flapping.
	flapping := 0.0
	if math.Mod(t, 660) < 240 {
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
