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

	result, err := coverage.Run(ctx, api, cfg, time.Now().UTC())
	if err != nil {
		t.Fatalf("coverage.Run, is the demo stack up: %v", err)
	}
	if len(result.ParseErrors) != 0 {
		t.Fatalf("unexpected parse errors: %v", result.ParseErrors)
	}
	grid := result.Grid

	byName := map[string]coverage.ServiceCoverage{}
	for _, sc := range grid {
		byName[sc.Service.Name] = sc
	}

	// demo/prometheus/rules/coverage.yml's TargetDown (`up == 0`) is the
	// fixture's only global rule: no scoping selector, no aggregation. It
	// must be attributed to EVERY discovered service, with scope "global".
	// Attributing it to none -- which is what the stack did before -- turns
	// the most common alerting rule in existence into a cluster-wide false
	// blind spot, and nothing else in the demo exercises this path.
	for _, sc := range grid {
		var found *coverage.RuleMatch
		for i, m := range sc.Covered[coverage.SignalRate] {
			if m.AlertName == "TargetDown" {
				found = &sc.Covered[coverage.SignalRate][i]
			}
		}
		if found == nil {
			t.Errorf("%s: TargetDown (`up == 0`) is not attributed to this service; a global rule covers all of them",
				sc.Service.Name)
			continue
		}
		if found.Scope != "global" {
			t.Errorf("%s: TargetDown Scope = %q, want global", sc.Service.Name, found.Scope)
		}
		if !found.Certain {
			t.Errorf("%s: TargetDown must classify as rate with certainty (`up` is the standard availability metric)",
				sc.Service.Name)
		}
	}

	// Prometheus's own self-scrape is excluded by default (config.Coverage.
	// ExcludeJobs) precisely because it would otherwise dominate this report
	// -- it must not appear at all, and Run must say it was left out.
	if _, ok := byName["prometheus"]; ok {
		t.Error("prometheus: expected to be excluded by default, found in the grid")
	}
	foundExcluded := false
	for _, j := range result.ExcludedJobs {
		if j == "prometheus" {
			foundExcluded = true
		}
	}
	if !foundExcluded {
		t.Errorf("ExcludedJobs = %v, want \"prometheus\" present", result.ExcludedJobs)
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
	// Saturation-only as far as rules NAMING billing go; rate is covered
	// only by the cluster-wide TargetDown, asserted above.
	for _, sig := range []coverage.Signal{coverage.SignalErrors, coverage.SignalLatency} {
		if billing.Covers(sig) {
			t.Errorf("shop/billing: unexpected coverage on %s (should be saturation-only)", sig)
		}
	}
	for _, m := range billing.Covered[coverage.SignalRate] {
		if m.Scope == "matched" {
			t.Errorf("shop/billing: unexpected service-scoped rate rule %q (should be saturation-only)", m.AlertName)
		}
	}

	search, ok := byName["search"]
	if !ok {
		t.Fatal("search not discovered; is demo/prometheus/prometheus.yml's search job scraping")
	}
	if search.Service.Source != "job" {
		t.Errorf("search Source = %q, want job (no SD labels)", search.Service.Source)
	}
	if search.AnyScopedCoverage() {
		t.Error("search: expected no rule to name it, it is the headline blind spot")
	}
	if search.Service.TrafficBasis != coverage.BasisRequests {
		t.Errorf("search TrafficBasis = %q, want %q (it exposes http_requests_total)",
			search.Service.TrafficBasis, coverage.BasisRequests)
	}

	batchworker, ok := byName["batchworker"]
	if !ok {
		t.Fatal("batchworker not discovered")
	}
	if batchworker.AnyScopedCoverage() {
		t.Error("batchworker: expected no rule to name it")
	}
	if batchworker.Service.TrafficBasis != coverage.BasisSamples {
		t.Errorf("batchworker TrafficBasis = %q, want %q (no request counter exposed)",
			batchworker.Service.TrafficBasis, coverage.BasisSamples)
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
