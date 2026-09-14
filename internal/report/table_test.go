package report

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/score"
)

func testMeta() Meta {
	return Meta{
		WindowStart: time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
		RulesActive: 2, Episodes: 300, Silences: 4,
		SilencesAvailable: true,
	}
}

func TestRenderSortsByNoiseDescending(t *testing.T) {
	rows := []Row{
		{AlertName: "Quiet", Noise: 12, Confidence: 1, Verdict: score.VerdictKeep},
		{AlertName: "Loud", Noise: 88, Confidence: 1, Verdict: score.VerdictRetire},
	}
	var sb strings.Builder
	if err := Render(&sb, rows, testMeta()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	if strings.Index(out, "Loud") > strings.Index(out, "Quiet") {
		t.Error("rows must be ordered by noise score, highest first")
	}
}

func TestRenderHandlesNoRules(t *testing.T) {
	var sb strings.Builder
	if err := Render(&sb, nil, testMeta()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(sb.String(), "no rules scored") {
		t.Errorf("empty result must be explicit, got:\n%s", sb.String())
	}
}

func TestRenderIncludesEvidenceColumns(t *testing.T) {
	rows := []Row{{
		AlertName: "DemoSpiky", GroupName: "demo",
		Noise: 87, Confidence: 0.9, Verdict: score.VerdictRetire,
		Signals: score.Signals{
			Fires: 214, ShortLivedRate: 0.94,
			SilencedRate: 0.31, FlapRate: 0.12,
		},
	}}
	var sb strings.Builder
	if err := Render(&sb, rows, testMeta()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{"DemoSpiky", "retire", "214", "94%", "31%"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderIncludesChurnAndP50 pins issue #3: pending_churn can flip a
// verdict from retire to tune, and P50Duration is one of three conditions
// for automate, but neither had a column -- a rule could show every other
// signal pointing at delete and still land on `tune` with nothing on screen
// explaining why.
func TestRenderIncludesChurnAndP50(t *testing.T) {
	rows := []Row{{
		AlertName: "DemoFlapping", GroupName: "demo",
		Noise: 51, Confidence: 1, Verdict: score.VerdictTune,
		Signals: score.Signals{
			Fires: 3667, P50Duration: 3 * time.Minute,
			PendingChurn: 0.62,
		},
	}}
	var sb strings.Builder
	if err := Render(&sb, rows, testMeta()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{"CHURN", "P50", "62%", "3m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{3 * time.Minute, "3m"},
		{time.Hour + 2*time.Minute, "1h2m"},
		{time.Hour, "1h"},
		{time.Hour + 5*time.Second, "1h0m5s"},
		{90 * time.Second, "1m30s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderShowsSilencesUnavailable(t *testing.T) {
	meta := testMeta()
	meta.SilencesAvailable = false
	meta.Silences = 0

	var sb strings.Builder
	if err := Render(&sb, nil, meta); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "unavailable (silenced_rate reads 0 for every rule)") {
		t.Errorf("unavailable silences must say so, not print a count that looks like a measurement, got:\n%s", out)
	}
	if strings.Contains(out, "Silences   4") {
		t.Error("must not print a silence count when the signal was unavailable")
	}
}

// TestRenderShowsStoredSilencesWhenAlertmanagerUnavailable guards the state
// that was previously misreported: Alertmanager is down, but the store still
// holds silences from an earlier scan, so silenced_rate is NOT necessarily
// zero and the report must not claim it is unavailable outright.
func TestRenderShowsStoredSilencesWhenAlertmanagerUnavailable(t *testing.T) {
	meta := testMeta()
	meta.SilencesAvailable = false
	meta.Silences = 4

	var sb strings.Builder
	if err := Render(&sb, nil, meta); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Silences   4 (from store; alertmanager unavailable, newer silences may be missing)") {
		t.Errorf("stored silences during an outage must say so, not claim they are unavailable, got:\n%s", out)
	}
	if strings.Contains(out, "unavailable (silenced_rate reads 0 for every rule)") {
		t.Error("must not claim silenced_rate reads 0 when the store still holds silences")
	}
}

// TestRenderBreaksTiesByGroupThenName guards determinism. sort.Slice is not
// stable and alert names are not unique across groups, so without a final
// tiebreak two equally-noisy rules with the same name in different groups
// could swap places between runs of the same scan.
func TestRenderBreaksTiesByGroupThenName(t *testing.T) {
	rows := []Row{
		{AlertName: "Shared", GroupName: "zzz", Noise: 50, Verdict: score.VerdictKeep},
		{AlertName: "Shared", GroupName: "aaa", Noise: 50, Verdict: score.VerdictKeep},
	}
	var sb strings.Builder
	if err := Render(&sb, rows, testMeta()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	if strings.Index(out, "aaa") > strings.Index(out, "zzz") {
		t.Error("equal noise and name must tiebreak on group name, ascending")
	}
}

func TestPctClampsAndHandlesNaN(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{math.NaN(), "-"},
		{-0.5, "0%"},
		{1.5, "100%"},
		{0.5, "50%"},
	}
	for _, c := range cases {
		if got := pct(c.in); got != c.want {
			t.Errorf("pct(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRenderTruncatedWindowReportsWhereDataBegins pins the corrected wording.
// The scan cannot tell a retention edge from a rule set younger than the
// window, so it states the measured fact -- where data begins -- instead of
// claiming retention clipped anything.
func TestRenderTruncatedWindowReportsWhereDataBegins(t *testing.T) {
	meta := testMeta()
	meta.Truncated = true
	meta.EarliestData = time.Date(2026, 9, 8, 4, 30, 0, 0, time.UTC)

	var sb strings.Builder
	if err := Render(&sb, nil, meta); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	want := "Window     2026-08-15 to 2026-09-14  (30d requested, data begins 2026-09-08)"
	if !strings.Contains(out, want) {
		t.Errorf("window line = \n%s\nwant it to contain:\n%s", out, want)
	}
	if strings.Contains(out, "limited by retention") {
		t.Error("the report must not claim retention limited the window; it cannot know that")
	}
}

// A truncated flag with no EarliestData cannot say where data begins, so it
// must fall back to the plain line rather than print a zero date.
func TestRenderTruncatedWithoutEarliestDataFallsBack(t *testing.T) {
	meta := testMeta()
	meta.Truncated = true

	var sb strings.Builder
	if err := Render(&sb, nil, meta); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(sb.String(), "(30d)") {
		t.Errorf("want the plain window line, got:\n%s", sb.String())
	}
}

// TestRenderStatesAmbiguousNames pins the header line. Ambiguous rules are
// skipped entirely, so without this the report silently omits rules the user
// can see in Prometheus.
func TestRenderStatesAmbiguousNames(t *testing.T) {
	meta := testMeta()
	meta.Ambiguous = 2

	var sb strings.Builder
	if err := Render(&sb, nil, meta); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Ambiguous  2 alert names defined in more than one group (not scored)") {
		t.Errorf("ambiguous names must be stated in the header, got:\n%s", out)
	}

	meta.Ambiguous = 1
	sb.Reset()
	if err := Render(&sb, nil, meta); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(sb.String(), "1 alert name defined") {
		t.Errorf("singular case must read naturally, got:\n%s", sb.String())
	}

	meta.Ambiguous = 0
	sb.Reset()
	if err := Render(&sb, nil, meta); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(sb.String(), "Ambiguous") {
		t.Errorf("no ambiguous names must print no line, got:\n%s", sb.String())
	}
}
