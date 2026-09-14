//go:build integration

package e2e

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect"
	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// TestScanReachesExpectedVerdicts runs the full pipeline against the seeded
// demo stack. The demo rules are built so their correct verdicts are known in
// advance; this asserts scoring actually reaches them.
func TestScanReachesExpectedVerdicts(t *testing.T) {
	cfg := config.Default()
	cfg.Prometheus.URL = "http://localhost:9090"
	cfg.Alertmanager.URL = "http://localhost:9093"
	cfg.Timezone = "UTC"
	cfg.Database = filepath.Join(t.TempDir(), "e2e.db")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	// A scan issues one range query per chunk -- 120 of them over a 30-day
	// window at the default 6h chunk. Without a deadline a hung Prometheus
	// hangs the command forever, and this is a thing people run in CI.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout.Std())
	defer cancel()

	db, err := store.Open(cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	api, err := prom.New(cfg.Prometheus)
	if err != nil {
		t.Fatalf("prom client: %v", err)
	}

	groups, err := api.Rules(ctx)
	if err != nil {
		t.Fatalf("rules, is the demo stack up: %v", err)
	}
	now := time.Now().UTC()
	if _, err := collect.SyncRules(ctx, groups, db, now, cfg.Rules.MaxDeactivatedFraction, nil); err != nil {
		t.Fatalf("sync rules: %v", err)
	}

	res, err := collect.New(api, db, cfg).Run(ctx, now.Add(-cfg.Window.Std()), now)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Episodes == 0 {
		t.Fatal("no episodes backfilled; run `make demo-seed` first")
	}

	// A rule nobody ever silenced should not be retired on self-resolution
	// alone, so DemoSpiky needs silence evidence covering the seeded window.
	//
	// That evidence CANNOT come from Alertmanager's API: it refuses a silence
	// whose endsAt is in the past ("end time can't be in the past") and
	// silently clamps a past startsAt to now. Verified against a live
	// Alertmanager. Historical silences therefore only ever exist in
	// noisefloor's own store, which is faithful to production -- noisefloor
	// persists silences as it observes them, and keeps them after Alertmanager
	// has expired and garbage-collected them.
	//
	// So: seed the historical silence directly into the store, and separately
	// assert that FetchSilences still parses a live Alertmanager correctly.
	silences := []store.Silence{{
		AMID:      "e2e-demospiky",
		Matchers:  []store.Matcher{{Name: "alertname", Value: "DemoSpiky", IsEqual: true}},
		CreatedBy: "noisefloor-e2e",
		Comment:   "seeded historical silence",
		StartsAt:  res.WindowStart,
		EndsAt:    res.WindowEnd,
	}}
	if err := db.UpsertSilences(ctx, silences); err != nil {
		t.Fatalf("seed historical silence: %v", err)
	}

	// Separately prove the live path works, without depending on its times.
	if _, err := collect.FetchSilences(ctx, cfg.Alertmanager.URL, nil); err != nil {
		t.Fatalf("FetchSilences against the live Alertmanager: %v", err)
	}

	rules, _ := db.ListRules(ctx)
	all, _ := db.ListEpisodesInWindow(ctx, res.WindowStart, res.WindowEnd)
	byRule := map[int64][]store.Episode{}
	for _, e := range all {
		byRule[e.RuleID] = append(byRule[e.RuleID], e)
	}

	// Same rule as cmd/noisefloor: confidence is earned against time actually
	// observed, so a window whose data begins late is measured from where the
	// data begins, not from where the request did.
	window := res.WindowEnd.Sub(res.WindowStart)
	if res.Truncated && !res.EarliestData.IsZero() {
		window = res.WindowEnd.Sub(res.EarliestData)
	}
	verdicts := map[string]string{}
	signals := map[string]score.Signals{}

	for _, r := range rules {
		eps := byRule[r.ID]
		if len(eps) == 0 {
			continue
		}
		s := score.Compute(score.Input{
			Rule: r, Episodes: eps, AllEpisodes: all,
			Silences: silences, Location: time.UTC,
			FlapWindow: cfg.FlapWindow.Std(),
		})
		_, _, v := score.Evaluate(s, r, window, now, cfg)
		verdicts[r.AlertName] = v
		signals[r.AlertName] = s
	}

	// DemoSpiky already reaches `retire` on self-resolution alone once the
	// window is long enough -- the seeded silence only pushes the score
	// higher, it is not required to cross the threshold. Assert `retire`
	// either way; what matters is that the silence evidence was fed through
	// the pipeline correctly (proven above by not erroring), not which signal
	// happened to carry the verdict.
	if got := verdicts["DemoSpiky"]; got != score.VerdictRetire {
		t.Errorf("DemoSpiky verdict = %q, want retire; it self-resolves within "+
			"2m, does not flap, and is silenced across the window "+
			"(signals: %+v)", got, signals["DemoSpiky"])
	}
	if got := verdicts["DemoFlapping"]; got != score.VerdictTune {
		t.Errorf("DemoFlapping verdict = %q, want tune", got)
	}
	// DemoSustained is the project's ONLY fixture exercising the automate arm
	// (automateMinFires / automateMaxShort / automateMinDuration). This build
	// tag never runs in CI, so weakening this to `!= retire` would leave that
	// arm with zero automated regression coverage anywhere.
	if got := verdicts["DemoSustained"]; got != score.VerdictAutomate {
		t.Errorf("DemoSustained verdict = %q, want automate; it is real, "+
			"frequent and long-running without self-resolving (signals: %+v)",
			got, signals["DemoSustained"])
	}
	if _, scored := verdicts["DemoQuiet"]; scored {
		t.Error("DemoQuiet never fires and must not appear in results")
	}

	for _, name := range []string{"DemoCauseA", "DemoCauseB", "DemoCauseC", "DemoCauseD"} {
		s, ok := signals[name]
		if !ok {
			t.Errorf("%s produced no episodes; the seeder should have fired it", name)
			continue
		}
		if s.CofireRatio < 0.5 {
			t.Errorf("%s cofire_ratio = %.2f, want > 0.5; all four fire together",
				name, s.CofireRatio)
		}
	}
}
