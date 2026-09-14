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

	// Every waveform below has an on-period of at least two sample steps and
	// an off-period longer than the collector's two-step gap tolerance.
	// Shorter pulses alias away at a 1m step, or merge into one long episode,
	// and the demo then fails to demonstrate the thing it exists to show.
	// These periods must stay in step with demo/seed/main.go.

	// Spiky: 2m firing every 90m. Short-lived, and far enough apart that it
	// does not read as flapping.
	spiky := 0.0
	if math.Mod(t, 5400) < 120 {
		spiky = 1
	}

	// Flapping: 4m on, 4m off. Short episodes that keep coming back.
	flapping := 0.0
	if math.Mod(t, 480) < 240 {
		flapping = 1
	}

	// Sustained: one long episode per hour in the live stack, so the demo
	// shows something without waiting a day.
	sustained := 0.0
	if math.Mod(t, 3600) < 1800 {
		sustained = 1
	}

	// Cause A-D: four components that always breach together, which is what
	// cause-based alerting looks like from the outside. Four, not three,
	// because co-fire detection requires three OTHER rules firing alongside.
	cause := 0.0
	if math.Mod(t, 600) < 120 {
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
