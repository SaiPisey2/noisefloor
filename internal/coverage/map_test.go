package coverage

import "testing"

func testServices() []Service {
	return []Service{
		{Name: "shop/checkout", Namespace: "shop", Service: "checkout", Job: "checkout", Source: "k8s", Traffic: 500},
		{Name: "shop/billing", Namespace: "shop", Service: "billing", Job: "billing", Source: "k8s", Traffic: 300},
		{Name: "search", Job: "search", Source: "job", Traffic: 9000},
		{Name: "batchworker", Job: "batchworker", Source: "job", Traffic: 2},
	}
}

// TestMapRulesKnownByConstruction mirrors the demo fixture (issue #10, PART
// 1): checkout fully covered, billing saturation-only, search and
// batchworker with no rules mentioning them at all. The coverage outcome is
// knowable by construction, same as the scan archetypes are.
func TestMapRulesKnownByConstruction(t *testing.T) {
	rules := []Rule{
		{GroupName: "services", AlertName: "CheckoutErrorRateHigh", Expr: `
			sum(rate(http_requests_total{service="checkout",code=~"5.."}[5m]))
			/ sum(rate(http_requests_total{service="checkout"}[5m])) > 0.05`},
		{GroupName: "services", AlertName: "CheckoutLatencyHigh",
			Expr: `histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{service="checkout"}[5m])) by (le)) > 0.5`},
		{GroupName: "services", AlertName: "CheckoutMemoryHigh", Expr: `process_resident_memory_bytes{service="checkout"} > 5e8`},
		{GroupName: "services", AlertName: "CheckoutNoTraffic", Expr: `absent(http_requests_total{service="checkout"})`},
		{GroupName: "services", AlertName: "BillingMemoryHigh", Expr: `process_resident_memory_bytes{service="billing"} > 5e8`},
	}

	grid, parseErrors := MapRules(rules, testServices())
	if len(parseErrors) != 0 {
		t.Fatalf("unexpected parse errors: %v", parseErrors)
	}

	byName := map[string]ServiceCoverage{}
	for _, sc := range grid {
		byName[sc.Service.Name] = sc
	}

	checkout := byName["shop/checkout"]
	for _, sig := range []Signal{SignalRate, SignalErrors, SignalLatency, SignalSaturation} {
		if !checkout.Covers(sig) {
			t.Errorf("checkout: expected coverage on %s", sig)
		}
	}
	if checkout.Covers(SignalBurnRate) {
		t.Error("checkout: unexpected burn-rate coverage")
	}

	billing := byName["shop/billing"]
	if !billing.Covers(SignalSaturation) {
		t.Error("billing: expected saturation coverage")
	}
	for _, sig := range []Signal{SignalRate, SignalErrors, SignalLatency, SignalBurnRate} {
		if billing.Covers(sig) {
			t.Errorf("billing: unexpected coverage on %s (should be partial: saturation only)", sig)
		}
	}

	if byName["search"].AnyCoverage() {
		t.Error("search: expected zero coverage (the headline blind spot)")
	}
	if byName["batchworker"].AnyCoverage() {
		t.Error("batchworker: expected zero coverage (idle, but still uncovered)")
	}
}

func TestMapRulesGlobalAggregation(t *testing.T) {
	rules := []Rule{
		{GroupName: "org", AlertName: "AnyServiceDown", Expr: `sum by (job) (up) == 0`},
	}
	grid, _ := MapRules(rules, testServices())
	for _, sc := range grid {
		if !sc.Covers(SignalRate) {
			t.Errorf("%s: expected global `by (job)` rule to cover rate", sc.Service.Name)
		}
		for _, m := range sc.Covered[SignalRate] {
			if m.Scope != "global" {
				t.Errorf("%s: Scope = %q, want global", sc.Service.Name, m.Scope)
			}
		}
	}
}

func TestMapRulesUnattributedMatcher(t *testing.T) {
	// Names a service nothing was discovered under: must attribute to
	// nothing, not fall back to "every service".
	rules := []Rule{
		{GroupName: "g", AlertName: "GhostErrors", Expr: `rate(http_requests_total{service="ghost",code=~"5.."}[5m]) > 0`},
	}
	grid, _ := MapRules(rules, testServices())
	for _, sc := range grid {
		if sc.AnyCoverage() {
			t.Errorf("%s: unexpected coverage from a rule naming an undiscovered service", sc.Service.Name)
		}
	}
}

func TestMapRulesParseError(t *testing.T) {
	rules := []Rule{{GroupName: "g", AlertName: "Broken", Expr: `sum(rate(http_requests_total[5m])`}}
	grid, parseErrors := MapRules(rules, testServices())
	if len(parseErrors) != 1 {
		t.Fatalf("got %d parse errors, want 1", len(parseErrors))
	}
	if _, ok := parseErrors["g/Broken"]; !ok {
		t.Errorf("parseErrors = %v, want key \"g/Broken\"", parseErrors)
	}
	for _, sc := range grid {
		if sc.AnyCoverage() {
			t.Errorf("%s: a rule that failed to parse must not attribute coverage", sc.Service.Name)
		}
	}
}
