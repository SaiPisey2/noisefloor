package main

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateProducesOpenMetrics(t *testing.T) {
	end := time.Unix(1_700_000_000, 0).UTC()
	start := end.Add(-24 * time.Hour)

	var sb strings.Builder
	if err := Generate(&sb, start, end, time.Minute); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := sb.String()

	if !strings.HasPrefix(out, "# TYPE ALERTS gauge\n") {
		t.Errorf("missing TYPE header, got prefix %q", out[:min(40, len(out))])
	}
	if !strings.HasSuffix(out, "# EOF\n") {
		t.Error("OpenMetrics output must end with # EOF")
	}
	for _, want := range []string{
		`alertname="DemoSpiky"`,
		`alertname="DemoFlapping"`,
		`alertname="DemoSustained"`,
		`alertname="DemoCauseA"`,
		`alertname="DemoCauseD"`,
		`alertstate="firing"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s", want)
		}
	}
	if strings.Contains(out, "DemoQuiet") {
		t.Error("DemoQuiet must never appear; it is the never-fires control")
	}

	// The seeded series must carry the same labels the live rule produces,
	// or a seeded episode becomes a second fingerprint for the same rule and
	// concentration reads as an artefact of the fixture instead of the rule.
	var causeLine, spikyLine string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, `alertname="DemoCauseA"`):
			causeLine = line
		case strings.Contains(line, `alertname="DemoSpiky"`):
			spikyLine = line
		}
	}
	if causeLine == "" {
		t.Fatal("no DemoCauseA sample line found")
	}
	if !strings.Contains(causeLine, `component="a"`) {
		t.Errorf("DemoCauseA sample missing component label, got: %s", causeLine)
	}
	if spikyLine == "" {
		t.Fatal("no DemoSpiky sample line found")
	}
	if strings.Contains(spikyLine, "component=") {
		t.Errorf("DemoSpiky's live rule adds no component label; seeded sample must not either, got: %s", spikyLine)
	}
}

// TestScenarioOffPeriodsExceedGapTolerance guards the property the whole demo
// rests on. The collector merges samples separated by two steps or less, so a
// scenario whose off-period is shorter than that produces one unbroken episode
// and silently stops demonstrating flapping or self-resolution.
func TestScenarioOffPeriodsExceedGapTolerance(t *testing.T) {
	const step = 60.0
	const tolerance = 2 * step

	for _, sc := range scenarios() {
		var longestOff, currentOff float64
		var sawOn bool

		for tt := 0.0; tt < 7*86400; tt += step {
			if sc.firing(tt) {
				sawOn = true
				if currentOff > longestOff {
					longestOff = currentOff
				}
				currentOff = 0
				continue
			}
			if sawOn {
				currentOff += step
			}
		}
		if !sawOn {
			t.Errorf("%s never fires in a week of samples", sc.alertname)
			continue
		}
		if longestOff <= tolerance {
			t.Errorf("%s has no off-period longer than %.0fs; every episode "+
				"will merge into one and the scenario proves nothing",
				sc.alertname, tolerance)
		}
	}
}

func TestGenerateSamplesAreOrderedPerSeries(t *testing.T) {
	end := time.Unix(1_700_000_000, 0).UTC()
	var sb strings.Builder
	if err := Generate(&sb, end.Add(-6*time.Hour), end, time.Minute); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	lastTS := map[string]float64{}
	for _, line := range strings.Split(sb.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series, ts, ok := splitSample(line)
		if !ok {
			t.Fatalf("unparseable sample line: %q", line)
		}
		if prev, seen := lastTS[series]; seen && ts <= prev {
			t.Fatalf("series %s has non-increasing timestamps: %v then %v", series, prev, ts)
		}
		lastTS[series] = ts
	}
	if len(lastTS) == 0 {
		t.Fatal("no samples generated")
	}
}
