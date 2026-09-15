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

	grid, parseErrors, _ := MapRules(rules, testServices())
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
	grid, _, _ := MapRules(rules, testServices())
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
	grid, _, _ := MapRules(rules, testServices())
	for _, sc := range grid {
		if sc.AnyCoverage() {
			t.Errorf("%s: unexpected coverage from a rule naming an undiscovered service", sc.Service.Name)
		}
	}
}

// TestMapRulesGlobalUnaggregatedRule is the `up == 0` case: the single most
// common global rule in existence, carrying no scoping selector and no
// aggregation. It used to attribute to NO service, so a Prometheus whose
// only cluster-wide rule was this one reported every service as having no
// rate coverage -- a fleet-wide blind spot manufactured out of a rule that
// covers everything.
func TestMapRulesGlobalUnaggregatedRule(t *testing.T) {
	rules := []Rule{{GroupName: "org", AlertName: "TargetDown", Expr: `up == 0`}}
	grid, parseErrors, unattributed := MapRules(rules, testServices())
	if len(parseErrors) != 0 {
		t.Fatalf("unexpected parse errors: %v", parseErrors)
	}
	if len(unattributed) != 0 {
		t.Errorf("unattributed = %v, want empty: `up == 0` covers every service", unattributed)
	}
	for _, sc := range grid {
		if !sc.Covers(SignalRate) {
			t.Errorf("%s: expected `up == 0` to cover rate", sc.Service.Name)
		}
		for _, m := range sc.Covered[SignalRate] {
			if m.Scope != "global" {
				t.Errorf("%s: Scope = %q, want global", sc.Service.Name, m.Scope)
			}
		}
		// Global coverage is real, but it is not alerting on this service:
		// it must not clear the service off the blind-spot list.
		if sc.AnyScopedCoverage() {
			t.Errorf("%s: a global rule must not count as scoped coverage", sc.Service.Name)
		}
	}
}

// TestMapRulesFallsThroughAnUnresolvableScope covers the two remaining
// early-return cases: a namespace regexp that matches nothing discovered,
// and an exporter's own job name (kube-state-metrics, cAdvisor and every
// other shared exporter put ITS job on the series, never the workload's).
// Both used to stop the search dead at the first scoping label present and
// attribute the rule to nobody.
func TestMapRulesFallsThroughAnUnresolvableScope(t *testing.T) {
	rules := []Rule{
		{GroupName: "org", AlertName: "ProdErrors",
			Expr: `sum by (namespace) (rate(http_requests_total{namespace=~"prod.*",code=~"5.."}[5m])) > 1`},
		{GroupName: "org", AlertName: "ContainerCPUHigh",
			Expr: `sum by (namespace) (rate(container_cpu_usage_seconds_total{job="cadvisor"}[5m])) > 1`},
	}
	grid, _, unattributed := MapRules(rules, testServices())
	if len(unattributed) != 0 {
		t.Errorf("unattributed = %v, want empty: both rules aggregate by namespace", unattributed)
	}
	for _, sc := range grid {
		if !sc.Covers(SignalErrors) {
			t.Errorf("%s: expected the prod.* rule to fall through to its `by (namespace)` grouping", sc.Service.Name)
		}
		if !sc.Covers(SignalSaturation) {
			t.Errorf("%s: expected the cAdvisor rule to fall through past its exporter job", sc.Service.Name)
		}
	}
}

// TestMapRulesScopedByUnrecognisedLabelIsNotGlobal is N3: a rule narrowed
// by a label this package does not treat as scoping (mountpoint, instance,
// ...) is scoped to SOMETHING, even though MapRules cannot say what -- it
// must not be credited as `global` coverage for every discovered service.
// Before this fix, spansEveryService's "scoped" test only looked at
// job/namespace/service matchers, so this exact rule -- unaggregated and
// narrowed only by mountpoint/instance -- fell through as "not scoped" and
// was attributed to every service with Scope "global", manufacturing
// saturation coverage for services it says nothing about.
func TestMapRulesScopedByUnrecognisedLabelIsNotGlobal(t *testing.T) {
	rules := []Rule{
		{GroupName: "org", AlertName: "NodeDiskFull",
			Expr: `node_filesystem_avail_bytes{mountpoint="/",instance="node1"} < 1e9`},
	}
	grid, _, unattributed := MapRules(rules, testServices())
	if len(unattributed) != 1 || unattributed[0] != "org/NodeDiskFull" {
		t.Errorf("unattributed = %v, want [\"org/NodeDiskFull\"]: scoped to an unresolved instance/mountpoint, not global", unattributed)
	}
	for _, sc := range grid {
		if sc.AnyCoverage() {
			t.Errorf("%s: unexpected saturation coverage from a rule scoped to a specific mountpoint/instance", sc.Service.Name)
		}
	}
}

// TestMapRulesResolvesRegexpScopes: a regexp matcher that genuinely names
// discovered services must resolve to exactly those, with Scope "matched"
// -- not fall through to covering everything.
func TestMapRulesResolvesRegexpScopes(t *testing.T) {
	rules := []Rule{
		{GroupName: "g", AlertName: "ShopErrors", Expr: `rate(http_requests_total{namespace=~"sho.+",code=~"5.."}[5m]) > 1`},
	}
	grid, _, _ := MapRules(rules, testServices())
	for _, sc := range grid {
		want := sc.Service.Namespace == "shop"
		if got := sc.Covers(SignalErrors); got != want {
			t.Errorf("%s: Covers(errors) = %v, want %v", sc.Service.Name, got, want)
		}
		for _, m := range sc.Covered[SignalErrors] {
			if m.Scope != "matched" {
				t.Errorf("%s: Scope = %q, want matched", sc.Service.Name, m.Scope)
			}
		}
	}
}

// TestMapRulesReportsUnattributedRules is the counter that makes the
// difference above visible: a rule that parsed and classified but landed on
// nobody is not a coverage gap, and the grid renders both as "-".
func TestMapRulesReportsUnattributedRules(t *testing.T) {
	rules := []Rule{
		{GroupName: "g", AlertName: "GhostErrors", Expr: `rate(http_requests_total{service="ghost",code=~"5.."}[5m]) > 0`},
		{GroupName: "services", AlertName: "BillingMemoryHigh", Expr: `process_resident_memory_bytes{service="billing"} > 5e8`},
	}
	grid, _, unattributed := MapRules(rules, testServices())
	want := []string{"g/GhostErrors"}
	if len(unattributed) != 1 || unattributed[0] != want[0] {
		t.Errorf("unattributed = %v, want %v", unattributed, want)
	}
	for _, sc := range grid {
		if sc.Covers(SignalErrors) {
			t.Errorf("%s: a rule naming an undiscovered service must not attribute coverage", sc.Service.Name)
		}
	}
}

func TestMapRulesParseError(t *testing.T) {
	rules := []Rule{{GroupName: "g", AlertName: "Broken", Expr: `sum(rate(http_requests_total[5m])`}}
	grid, parseErrors, _ := MapRules(rules, testServices())
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

// TestMapRulesAggregatedUnresolvedJobIsNotGlobal is the real-world case the
// demo fixture could not produce, found running coverage against the public
// Prometheus demo: the node-exporter and Prometheus mixins.
//
// PrometheusErrorSendingAlertsToAnyAlertmanager narrows to `job="prometheus"`
// and wraps that in `min without (alertmanager) (...)`. Its four siblings
// carry the same selector with no aggregation and are correctly reported as
// unattributed when no discovered service has that job. This one was not:
// spansEveryService consulted `scoped` only on its unaggregated branch, so
// an aggregation over a rule that names a job turned it into `global` and
// credited error coverage to every service in the fleet -- six services that
// have no error alerting whatsoever.
//
// Aggregation does not widen a rule. A rule naming a job/namespace/service
// this package cannot resolve is scoped to something real either way.
func TestMapRulesAggregatedUnresolvedJobIsNotGlobal(t *testing.T) {
	rules := []Rule{
		{GroupName: "prometheus", AlertName: "PrometheusErrorSendingAlertsToAnyAlertmanager",
			Expr: `min without (alertmanager) (rate(prometheus_notifications_errors_total{alertmanager!~"",job="prometheus"}[5m]) / rate(prometheus_notifications_sent_total{alertmanager!~"",job="prometheus"}[5m])) * 100 > 3`},
	}
	grid, _, unattributed := MapRules(rules, testServices())
	want := "prometheus/PrometheusErrorSendingAlertsToAnyAlertmanager"
	if len(unattributed) != 1 || unattributed[0] != want {
		t.Errorf("unattributed = %v, want [%q]: names job=prometheus, which no discovered service has", unattributed, want)
	}
	for _, sc := range grid {
		if sc.AnyCoverage() {
			t.Errorf("%s: credited with coverage by a rule scoped to job=prometheus", sc.Service.Name)
		}
	}
}

// TestMapRulesAggregatedUnscopedStaysGlobal guards the other side of that
// fix: a rule carrying only a non-scoping matcher (a status code) really
// does span every service, and must still read as global. Blocking the
// global inference on ANY matcher would turn the most common error-rate
// rule there is into a fleet-wide false blind spot.
func TestMapRulesAggregatedUnscopedStaysGlobal(t *testing.T) {
	rules := []Rule{
		{GroupName: "org", AlertName: "ErrorRateHigh",
			Expr: `sum by (job) (rate(http_requests_total{code=~"5.."}[5m])) > 10`},
	}
	grid, _, unattributed := MapRules(rules, testServices())
	if len(unattributed) != 0 {
		t.Errorf("unattributed = %v, want none: the rule spans every job", unattributed)
	}
	for _, sc := range grid {
		if !sc.Covers(SignalErrors) {
			t.Errorf("%s: expected global error coverage", sc.Service.Name)
		}
	}
}
