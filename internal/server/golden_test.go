package server

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// assertGolden compares got against testdata/golden/name, updating the file
// instead of failing when UPDATE_GOLDEN=1 is set -- the same convention
// internal/pr's own golden tests use.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := "testdata/golden/" + name
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata/golden", 0o755); err != nil {
			t.Fatalf("mkdir testdata/golden: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden file %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file %s: %v (run with UPDATE_GOLDEN=1 to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("output does not match golden file %s.\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// goldenNow is a fixed clock every golden fixture below uses, so the
// rendered HTML (episode timeline positions, "as of" timestamps) never
// depends on the wall clock the test happens to run at.
var goldenNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func newGoldenServer(t *testing.T) (*Server, *store.SQLite) {
	t.Helper()
	db := newTestDB(t)
	srv := newTestServer(t, db)
	srv.nowFunc = func() time.Time { return goldenNow }
	return srv, db
}

func TestGoldenLeaderboard(t *testing.T) {
	srv, db := newGoldenServer(t)

	retire := seedRule(t, db, store.Rule{
		AlertName: "CauseySymptom", GroupName: "demo", Active: true, For: 0,
		FirstSeen: goldenNow.Add(-60 * 24 * time.Hour),
	})
	seedEpisode(t, db, retire, store.StateFiring, goldenNow.Add(-time.Hour), 3*time.Minute)
	seedScore(t, db, retire, store.Score{
		Verdict: score.VerdictRetire, NoiseScore: 45, Confidence: 0.8,
		WindowStart: goldenNow.Add(-30 * 24 * time.Hour), WindowEnd: goldenNow, ComputedAt: goldenNow,
		Signals: map[string]float64{
			"fires": 504, "p50_duration_s": 180, "short_lived_rate": 1,
			"silenced_rate": 0, "flap_rate": 0, "cofire_ratio": 1,
			"concentration": 0, "pending_churn": 0, "offhours_rate": 0.01,
		},
	})

	tune := seedRule(t, db, store.Rule{
		AlertName: "FlappingRule", GroupName: "demo", Active: true, For: 30 * time.Second,
		FirstSeen: goldenNow.Add(-60 * 24 * time.Hour),
	})
	seedEpisode(t, db, tune, store.StateFiring, goldenNow.Add(-2*time.Hour), 4*time.Minute)
	seedScore(t, db, tune, store.Score{
		Verdict: score.VerdictTune, NoiseScore: 48, Confidence: 0.8,
		WindowStart: goldenNow.Add(-30 * 24 * time.Hour), WindowEnd: goldenNow, ComputedAt: goldenNow,
		Signals: map[string]float64{
			"fires": 1186, "p50_duration_s": 240, "short_lived_rate": 0.91,
			"silenced_rate": 0, "flap_rate": 0.97, "cofire_ratio": 0.07,
			"concentration": 0, "pending_churn": 0, "offhours_rate": 0,
		},
	})

	body := get(t, srv.Handler(), "/").Body.String()
	assertGolden(t, "leaderboard.html", body)
}

func TestGoldenRuleDetailTune(t *testing.T) {
	srv, db := newGoldenServer(t)

	id := seedRule(t, db, store.Rule{
		AlertName: "FlappingRule", GroupName: "demo", Active: true, For: 30 * time.Second,
		Expr: `demo_flapping_gauge > 0`, File: "/etc/prometheus/rules/demo.yml", Line: 12,
		Labels:      map[string]string{"severity": "warning"},
		Annotations: map[string]string{"summary": "demo alert flaps on and off"},
		FirstSeen:   goldenNow.Add(-60 * 24 * time.Hour),
	})
	start := goldenNow.Add(-6 * time.Hour)
	for i := 0; i < 3; i++ {
		seedEpisode(t, db, id, store.StateFiring, start.Add(time.Duration(i)*2*time.Hour), 4*time.Minute)
	}
	seedScore(t, db, id, store.Score{
		Verdict: score.VerdictTune, NoiseScore: 48, Confidence: 0.8,
		WindowStart: goldenNow.Add(-30 * 24 * time.Hour), WindowEnd: goldenNow, ComputedAt: goldenNow,
		Signals: map[string]float64{
			"fires": 3, "unique_fingerprints": 1, "p50_duration_s": 240, "p90_duration_s": 240,
			"short_lived_rate": 0.91, "silenced_rate": 0, "flap_rate": 0.97, "cofire_ratio": 0.07,
			"concentration": 0, "pending_churn": 0, "offhours_rate": 0,
		},
	})

	body := get(t, srv.Handler(), "/rules/"+idString(id)).Body.String()
	assertGolden(t, "rule_detail_tune.html", body)
}

func TestGoldenRuleDetailNoScore(t *testing.T) {
	srv, db := newGoldenServer(t)
	id := seedRule(t, db, store.Rule{
		AlertName: "NeverFires", GroupName: "demo", Active: true, For: time.Minute,
		Expr: `up{job="quiet"} == 0`, FirstSeen: goldenNow.Add(-10 * 24 * time.Hour),
	})
	body := get(t, srv.Handler(), "/rules/"+idString(id)).Body.String()
	assertGolden(t, "rule_detail_no_score.html", body)
}

func TestGoldenCoverage(t *testing.T) {
	srv, db := newGoldenServer(t)

	snap := coverage.Snapshot{
		ComputedAt: goldenNow,
		Grid: []coverage.ServiceCoverage{
			{
				Service: coverage.Service{Name: "shop/checkout", Source: "k8s", Traffic: 12.5, TrafficBasis: coverage.BasisRequests},
				Covered: map[coverage.Signal][]coverage.RuleMatch{
					coverage.SignalRate:   {{GroupName: "services", AlertName: "CheckoutNoTraffic", Signal: coverage.SignalRate, Certain: true, Scope: "matched", Reason: "generic request/operation counter"}},
					coverage.SignalErrors: {{GroupName: "services", AlertName: "CheckoutErrorRateHigh", Signal: coverage.SignalErrors, Certain: true, Scope: "matched", Reason: "selector on code selects failure-like values"}},
				},
			},
			{
				Service: coverage.Service{Name: "search", Source: "job", Traffic: 40, TrafficBasis: coverage.BasisRequests},
				Covered: map[coverage.Signal][]coverage.RuleMatch{
					coverage.SignalRate: {{GroupName: "services", AlertName: "TargetDown", Signal: coverage.SignalRate, Certain: true, Scope: "global", Reason: "up is the standard Prometheus target-availability metric"}},
				},
			},
		},
		ExcludedJobs: []string{"prometheus"},
	}
	seedCoverageSnapshot(t, db, snap)

	body := get(t, srv.Handler(), "/coverage").Body.String()
	assertGolden(t, "coverage.html", body)
}

func idString(id int64) string {
	return strconv.FormatInt(id, 10)
}
