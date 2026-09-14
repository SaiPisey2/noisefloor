package report

import (
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

func TestRenderShowsTruncationWarning(t *testing.T) {
	meta := testMeta()
	meta.Truncated = true

	var sb strings.Builder
	if err := Render(&sb, nil, meta); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(sb.String(), "limited by retention") {
		t.Error("a truncated window must say so; silently reporting a shorter window is a lie")
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
