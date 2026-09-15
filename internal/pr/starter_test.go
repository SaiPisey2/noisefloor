package pr

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/remediate"
)

// searchService is the fixture used throughout: the demo's own headline
// blind spot (see internal/e2e/coverage_test.go and README's Coverage
// section), a job-discovered service with substantial measured traffic.
func searchService() coverage.Service {
	return coverage.Service{
		Name: "search", Job: "search", Source: "job", Up: true,
		Traffic: 40.12, TrafficBasis: coverage.BasisRequests,
	}
}

var starterTarget = Target{File: "testdata/starter_fixture.yml", Group: "services"}

// --- template golden tests -------------------------------------------------

func TestRateTemplateGolden(t *testing.T) {
	svc := searchService()
	m := starterMeasurement{Supported: true, MetricUsed: "http_requests_total", Selector: `job="search"`}
	rule := rateTemplate(svc, m)
	if err := verifyStarterClassification(coverage.SignalRate, rule); err != nil {
		t.Fatalf("template does not classify as its own signal: %v", err)
	}
	if rule.ThresholdDerived {
		t.Error("rate template has nothing to derive; ThresholdDerived must be false")
	}
	body := StarterBody(svc, coverage.SignalRate, rule, m, starterTarget)
	assertGolden(t, "testdata/golden/starter_rate.golden.md", body)
}

func TestErrorsTemplateGoldenDerived(t *testing.T) {
	svc := searchService()
	m := starterMeasurement{
		Supported: true, MetricUsed: "http_requests_total", Selector: `job="search"`, CodeLabel: "code",
		Current: 0.03, Derived: true,
	}
	rule := errorsTemplate(svc, m)
	if err := verifyStarterClassification(coverage.SignalErrors, rule); err != nil {
		t.Fatalf("template does not classify as its own signal: %v", err)
	}
	if !rule.ThresholdDerived {
		t.Error("expected the threshold to be reported as derived from a nonzero current ratio")
	}
	body := StarterBody(svc, coverage.SignalErrors, rule, m, starterTarget)
	assertGolden(t, "testdata/golden/starter_errors.golden.md", body)
}

// TestErrorsTemplateFloorWhenNoTraffic covers the other branch: no requests
// to measure a baseline from, so the fixed floor is used and the body must
// say so rather than imply a measurement that never happened.
func TestErrorsTemplateFloorWhenNoTraffic(t *testing.T) {
	svc := searchService()
	m := starterMeasurement{
		Supported: true, MetricUsed: "http_requests_total", Selector: `job="search"`, CodeLabel: "code",
		Derived: false,
	}
	rule := errorsTemplate(svc, m)
	if rule.ThresholdDerived {
		t.Error("expected ThresholdDerived == false with no current ratio to scale")
	}
	if !strings.Contains(rule.Explained, "fixed default") {
		t.Errorf("explanation does not say the threshold is a fixed default:\n%s", rule.Explained)
	}
	if !strings.Contains(rule.Expr, formatPercentAsFraction(errorRatioFloor)) {
		t.Errorf("expr does not use the floor threshold: %s", rule.Expr)
	}
}

func TestLatencyTemplateGoldenDerived(t *testing.T) {
	svc := searchService()
	m := starterMeasurement{
		Supported: true, MetricUsed: "http_request_duration_seconds", Selector: `job="search"`,
		Current: 1.5, Derived: true,
	}
	rule := latencyTemplate(svc, m)
	if err := verifyStarterClassification(coverage.SignalLatency, rule); err != nil {
		t.Fatalf("template does not classify as its own signal: %v", err)
	}
	if !rule.ThresholdDerived {
		t.Error("expected the threshold to be reported as derived")
	}
	body := StarterBody(svc, coverage.SignalLatency, rule, m, starterTarget)
	assertGolden(t, "testdata/golden/starter_latency.golden.md", body)
}

func TestSaturationTemplateGoldenDerived(t *testing.T) {
	svc := searchService()
	m := starterMeasurement{
		Supported: true, MetricUsed: "process_resident_memory_bytes", Selector: `job="search"`,
		Current: 5.4e8, Derived: true,
	}
	rule := saturationTemplate(svc, m)
	if err := verifyStarterClassification(coverage.SignalSaturation, rule); err != nil {
		t.Fatalf("template does not classify as its own signal: %v", err)
	}
	if !rule.ThresholdDerived {
		t.Error("expected the threshold to be reported as derived")
	}
	body := StarterBody(svc, coverage.SignalSaturation, rule, m, starterTarget)
	assertGolden(t, "testdata/golden/starter_saturation.golden.md", body)
}

// --- BuildStarter / refusal tests -------------------------------------------

func TestBuildStarterRefusesIdle(t *testing.T) {
	in := StarterInput{
		Service: searchService(), Signal: coverage.SignalRate, Idle: true,
		Measurement: starterMeasurement{Supported: true},
		Target:      starterTarget,
	}
	p, refusal, err := BuildStarter(in)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal for an idle service")
	}
	if refusal == nil || refusal.Reason != ReasonIdle {
		t.Errorf("refusal = %+v, want ReasonIdle", refusal)
	}
}

func TestBuildStarterRefusesNoMetric(t *testing.T) {
	in := StarterInput{
		Service: searchService(), Signal: coverage.SignalLatency,
		Measurement: starterMeasurement{Supported: false},
		Target:      starterTarget,
	}
	p, refusal, err := BuildStarter(in)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal when the service exposes no supporting metric")
	}
	if refusal == nil || refusal.Reason != ReasonNoMetric {
		t.Errorf("refusal = %+v, want ReasonNoMetric", refusal)
	}
}

// TestBuildStarterRefusesUncertainCoverage exercises a condition that is
// structurally unreachable through RunStarters' own scope (a genuine blind
// spot, by RankBlindSpots' definition, has zero matches on every signal) --
// kept explicit and independently tested the same way pr.Build documents
// its own currently-unreachable confidence-floor check.
func TestBuildStarterRefusesUncertainCoverage(t *testing.T) {
	in := StarterInput{
		Service: searchService(), Signal: coverage.SignalErrors,
		ExistingMatches: []coverage.RuleMatch{{GroupName: "g", AlertName: "a", Signal: coverage.SignalErrors, Certain: false}},
		Measurement:     starterMeasurement{Supported: true},
		Target:          starterTarget,
	}
	p, refusal, err := BuildStarter(in)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal over an uncertain existing classification")
	}
	if refusal == nil || refusal.Reason != ReasonUncertainCoverage {
		t.Errorf("refusal = %+v, want ReasonUncertainCoverage", refusal)
	}
}

func TestBuildStarterIsNilNilWhenCertainlyCovered(t *testing.T) {
	in := StarterInput{
		Service: searchService(), Signal: coverage.SignalErrors,
		ExistingMatches: []coverage.RuleMatch{{GroupName: "g", AlertName: "a", Signal: coverage.SignalErrors, Certain: true}},
		Measurement:     starterMeasurement{Supported: true},
		Target:          starterTarget,
	}
	p, refusal, err := BuildStarter(in)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil || refusal != nil {
		t.Errorf("expected (nil, nil) for a genuinely covered signal, got (%v, %v)", p, refusal)
	}
}

// TestBuildStarterRefusesNoTarget is the "target file or group cannot be
// determined" case the spec calls out explicitly.
func TestBuildStarterRefusesNoTarget(t *testing.T) {
	in := StarterInput{
		Service: searchService(), Signal: coverage.SignalRate,
		Measurement: starterMeasurement{Supported: true, MetricUsed: "http_requests_total", Selector: `job="search"`},
		Target:      Target{},
	}
	p, refusal, err := BuildStarter(in)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no proposal with no resolvable target")
	}
	if refusal == nil || refusal.Reason != ReasonNoTarget {
		t.Errorf("refusal = %+v, want ReasonNoTarget", refusal)
	}
}

func TestBuildStarterProducesAProposal(t *testing.T) {
	in := StarterInput{
		Service: searchService(), Signal: coverage.SignalRate,
		Measurement: starterMeasurement{Supported: true, MetricUsed: "http_requests_total", Selector: `job="search"`},
		Target:      starterTarget,
	}
	p, refusal, err := BuildStarter(in)
	if err != nil {
		t.Fatalf("BuildStarter: %v", err)
	}
	if refusal != nil {
		t.Fatalf("unexpected refusal: %v", refusal)
	}
	if p == nil {
		t.Fatal("expected a proposal")
	}
	if p.Kind != KindPropose {
		t.Errorf("Kind = %q, want propose", p.Kind)
	}
	if p.AlertName != "SearchNoTraffic" {
		t.Errorf("AlertName = %q, want SearchNoTraffic", p.AlertName)
	}
	if p.Group != "services" || p.File != starterTarget.File {
		t.Errorf("proposal targets %s/%s, want services/%s", p.Group, p.File, starterTarget.File)
	}
	if err := p.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(p.NewContent, "- alert: SearchNoTraffic") {
		t.Errorf("rendered content missing the new rule:\n%s", p.NewContent)
	}
	if !strings.Contains(p.NewContent, "      - alert: SearchNoTraffic") {
		t.Errorf("new rule is not indented to match its sibling rules:\n%s", p.NewContent)
	}
	if !strings.Contains(p.Diff, "+      - alert: SearchNoTraffic") {
		t.Errorf("diff does not show the inserted rule:\n%s", p.Diff)
	}
	// The existing rules in the fixture must survive completely untouched.
	if !strings.Contains(p.NewContent, "CheckoutMemoryHigh") || !strings.Contains(p.NewContent, "UnrelatedThing") {
		t.Error("existing rules were disturbed by the insert")
	}
}

// --- resolveTarget -----------------------------------------------------

func fixtureLocations(t *testing.T) map[remediate.RuleKey]remediate.RuleLocation {
	t.Helper()
	locs, ferrs, err := remediate.LocateRules("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(ferrs) != 0 {
		t.Fatalf("unexpected file errors: %v", ferrs)
	}
	return remediate.LocationsByKey(locs)
}

func TestResolveTargetFindsExistingMatchedGroup(t *testing.T) {
	locByKey := fixtureLocations(t)
	sc := coverage.ServiceCoverage{
		Service: coverage.Service{Name: "checkout", Job: "checkout", Source: "job"},
		Covered: map[coverage.Signal][]coverage.RuleMatch{
			coverage.SignalSaturation: {{GroupName: "services", AlertName: "CheckoutMemoryHigh", Scope: "matched", Certain: true}},
		},
	}
	target := resolveTarget(sc, config.Default(), locByKey)
	if target.File != "testdata/starter_fixture.yml" || target.Group != "services" {
		t.Errorf("target = %+v, want testdata/starter_fixture.yml/services", target)
	}
}

func TestResolveTargetUsesConfigNomination(t *testing.T) {
	locByKey := fixtureLocations(t)
	sc := coverage.ServiceCoverage{Service: coverage.Service{Name: "search", Job: "search", Source: "job"}}
	cfg := config.Default()
	cfg.Coverage.RuleTargets = map[string]config.RuleTarget{
		"search": {File: "testdata/starter_fixture.yml", Group: "other"},
	}
	target := resolveTarget(sc, cfg, locByKey)
	if target.File != "testdata/starter_fixture.yml" || target.Group != "other" {
		t.Errorf("target = %+v, want testdata/starter_fixture.yml/other", target)
	}
}

// TestResolveTargetRefusesUnknownNomination is the "target file or group
// cannot be determined" case: a config nomination naming a group that does
// not actually exist in the checkout must not be trusted blindly.
func TestResolveTargetRefusesUnknownNomination(t *testing.T) {
	locByKey := fixtureLocations(t)
	sc := coverage.ServiceCoverage{Service: coverage.Service{Name: "search", Job: "search", Source: "job"}}
	cfg := config.Default()
	cfg.Coverage.RuleTargets = map[string]config.RuleTarget{
		"search": {File: "testdata/starter_fixture.yml", Group: "does-not-exist"},
	}
	target := resolveTarget(sc, cfg, locByKey)
	if target.File != "" || target.Group != "" {
		t.Errorf("target = %+v, want zero value for an unresolvable nomination", target)
	}
}

func TestResolveTargetZeroWhenNothingApplies(t *testing.T) {
	locByKey := fixtureLocations(t)
	sc := coverage.ServiceCoverage{Service: coverage.Service{Name: "search", Job: "search", Source: "job"}}
	target := resolveTarget(sc, config.Default(), locByKey)
	if target.File != "" || target.Group != "" {
		t.Errorf("target = %+v, want zero value with no matched group and no nomination", target)
	}
}

// --- verifyStarterClassification (metric-existence/consistency check) -----

func TestVerifyStarterClassificationRejectsWrongSignal(t *testing.T) {
	rule := starterRule{AlertName: "Bad", Expr: `rate(some_errors_total{job="x"}[5m]) > 1`}
	if err := verifyStarterClassification(coverage.SignalRate, rule); err == nil {
		t.Fatal("expected an error: the expression classifies as errors, not rate")
	}
}

func TestVerifyStarterClassificationAcceptsMatchingSignal(t *testing.T) {
	rule := starterRule{AlertName: "Good", Expr: `absent(http_requests_total{job="search"})`}
	if err := verifyStarterClassification(coverage.SignalRate, rule); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

// --- probes: metric existence against a fake Prometheus -------------------

// fakeInstantAPI implements prom.Client for probe tests, resolving Query by
// substring match against query so tests do not need to reproduce this
// package's exact PromQL formatting.
type fakeInstantAPI struct {
	respond func(query string) (model.Value, error)
}

func (f fakeInstantAPI) Query(_ context.Context, query string, _ time.Time) (model.Value, error) {
	return f.respond(query)
}
func (f fakeInstantAPI) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) (model.Matrix, error) {
	return nil, nil
}
func (f fakeInstantAPI) Rules(context.Context) ([]prom.RuleGroup, error) { return nil, nil }

var _ prom.Client = fakeInstantAPI{}

func vectorOf(labelSets ...model.Metric) model.Vector {
	vec := make(model.Vector, len(labelSets))
	for i, ls := range labelSets {
		vec[i] = &model.Sample{Metric: ls, Value: 1}
	}
	return vec
}

func TestProbeRateFindsExposedMetric(t *testing.T) {
	api := fakeInstantAPI{respond: func(query string) (model.Value, error) {
		if strings.Contains(query, "http_requests_total") {
			return vectorOf(model.Metric{"job": "search"}), nil
		}
		return model.Vector{}, nil
	}}
	m, err := probeRate(context.Background(), api, searchService(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !m.Supported || m.MetricUsed != "http_requests_total" {
		t.Errorf("m = %+v, want Supported with http_requests_total", m)
	}
}

func TestProbeRateUnsupportedWhenNoRequestCounterExists(t *testing.T) {
	api := fakeInstantAPI{respond: func(string) (model.Value, error) { return model.Vector{}, nil }}
	m, err := probeRate(context.Background(), api, searchService(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Supported {
		t.Errorf("expected Supported == false when no recognised request counter exists, got %+v", m)
	}
}

func TestProbeErrorsIgnoresGRPCCodeLabel(t *testing.T) {
	api := fakeInstantAPI{respond: func(query string) (model.Value, error) {
		if strings.Contains(query, "http_requests_total") && !strings.Contains(query, "rate(") {
			return vectorOf(model.Metric{"job": "search", "grpc_code": "0"}), nil
		}
		return model.Vector{}, nil
	}}
	m, err := probeErrors(context.Background(), api, searchService(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Supported {
		t.Errorf("expected Supported == false: only grpc_code was present, which errorsTemplate refuses to guess about, got %+v", m)
	}
}

func formatPercentAsFraction(f float64) string { return formatThreshold(f) }
