//go:build integration

package e2e

import (
	"context"
	"os"
	"os/signal"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/coverage"
)

// TestCoverageReachesExpectedGrid runs coverage.Run against the live demo
// stack. Its four coverage fixture services (demo/faultgen/coverage.go,
// demo/prometheus/rules/coverage.yml) are built so the correct grid is known
// in advance -- this asserts discovery, PromQL-based mapping and blind-spot
// ranking actually reach it end to end, against a real Prometheus.
func TestCoverageReachesExpectedGrid(t *testing.T) {
	cfg := config.Default()
	cfg.Prometheus.URL = "http://localhost:9090"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout.Std())
	defer cancel()

	api, err := prom.New(cfg.Prometheus)
	if err != nil {
		t.Fatalf("prom client: %v", err)
	}

	grid, parseErrors, err := coverage.Run(ctx, api, cfg, time.Now().UTC())
	if err != nil {
		t.Fatalf("coverage.Run, is the demo stack up: %v", err)
	}
	if len(parseErrors) != 0 {
		t.Fatalf("unexpected parse errors: %v", parseErrors)
	}

	byName := map[string]coverage.ServiceCoverage{}
	for _, sc := range grid {
		byName[sc.Service.Name] = sc
	}

	checkout, ok := byName["shop/checkout"]
	if !ok {
		t.Fatal("shop/checkout not discovered; is demo/prometheus/prometheus.yml's checkout job scraping")
	}
	if checkout.Service.Source != "k8s" {
		t.Errorf("shop/checkout Source = %q, want k8s", checkout.Service.Source)
	}
	for _, sig := range []coverage.Signal{coverage.SignalRate, coverage.SignalErrors, coverage.SignalLatency, coverage.SignalSaturation} {
		if !checkout.Covers(sig) {
			t.Errorf("shop/checkout: expected coverage on %s", sig)
		}
	}

	billing, ok := byName["shop/billing"]
	if !ok {
		t.Fatal("shop/billing not discovered")
	}
	if !billing.Covers(coverage.SignalSaturation) {
		t.Error("shop/billing: expected saturation coverage")
	}
	for _, sig := range []coverage.Signal{coverage.SignalRate, coverage.SignalErrors, coverage.SignalLatency} {
		if billing.Covers(sig) {
			t.Errorf("shop/billing: unexpected coverage on %s (should be saturation-only)", sig)
		}
	}

	search, ok := byName["search"]
	if !ok {
		t.Fatal("search not discovered; is demo/prometheus/prometheus.yml's search job scraping")
	}
	if search.Service.Source != "job" {
		t.Errorf("search Source = %q, want job (no SD labels)", search.Service.Source)
	}
	if search.AnyCoverage() {
		t.Error("search: expected zero coverage, it is the headline blind spot")
	}

	batchworker, ok := byName["batchworker"]
	if !ok {
		t.Fatal("batchworker not discovered")
	}
	if batchworker.AnyCoverage() {
		t.Error("batchworker: expected zero coverage")
	}

	blind := coverage.RankBlindSpots(grid)
	var names []string
	var searchIdx, batchIdx = -1, -1
	for i, b := range blind {
		names = append(names, b.Service.Name)
		switch b.Service.Name {
		case "search":
			searchIdx = i
			if b.Idle {
				t.Error("search: unexpectedly flagged Idle -- it carries the most traffic in the fixture")
			}
		case "batchworker":
			batchIdx = i
			if !b.Idle {
				t.Error("batchworker: expected Idle == true")
			}
		}
	}
	if searchIdx == -1 || batchIdx == -1 {
		t.Fatalf("blind spots = %v, want both search and batchworker present", names)
	}
	if searchIdx > batchIdx {
		t.Errorf("blind spot order = %v, want search ranked above batchworker (real traffic beats idle)", names)
	}
}
