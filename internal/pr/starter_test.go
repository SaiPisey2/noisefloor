package pr

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
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

// testScanWindow stands in for config.Config.Window in tests that call
// probeRate directly -- see quietWindowFor.
const testScanWindow = 30 * 24 * time.Hour

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

// TestErrorsTemplateCapsAnIncidentTimeRatio is the unfireable-threshold
// defect: 3x an incident-time ratio of 0.45 emitted `> 1.35`, a number a
// ratio can never reach, as a severity: page rule whose body called it
// measured.
func TestErrorsTemplateCapsAnIncidentTimeRatio(t *testing.T) {
	m := starterMeasurement{
		Supported: true, MetricUsed: "http_requests_total", Selector: `job="search"`, CodeLabel: "code",
		Current: 0.45, Derived: true,
	}
	rule := errorsTemplate(searchService(), m)
	if err := verifyStarterClassification(coverage.SignalErrors, rule); err != nil {
		t.Fatalf("template does not classify as its own signal: %v", err)
	}
	threshold := thresholdFromExpr(t, rule.Expr)
	if threshold >= 1.0 {
		t.Errorf("threshold = %v, want below 1.0: an error ratio can never exceed 1", threshold)
	}
	if threshold != errorRatioCeiling {
		t.Errorf("threshold = %v, want the %v ceiling", threshold, errorRatioCeiling)
	}
	// The number is no longer a multiple of anything measured, so the body
	// must not keep claiming it is.
	if rule.ThresholdDerived {
		t.Error("ThresholdDerived = true for a capped threshold; the cap is not derived from this service")
	}
	if !strings.Contains(rule.Explained, "ceiling") {
		t.Errorf("explanation does not disclose the cap:\n%s", rule.Explained)
	}
}

// TestErrorsTemplateGuardsAgainstLowTraffic is the "one 500 in a quiet
// window pages somebody" defect: the ratio alone is arithmetic on single
// events, so the rule must carry a denominator guard of its own.
func TestErrorsTemplateGuardsAgainstLowTraffic(t *testing.T) {
	m := starterMeasurement{
		Supported: true, MetricUsed: "http_requests_total", Selector: `job="search"`, CodeLabel: "code",
		Current: 0.01, Derived: true,
	}
	rule := errorsTemplate(searchService(), m)
	if rule.Severity != "page" {
		t.Fatalf("fixture assumption broken: Severity = %q, want page", rule.Severity)
	}
	guard := fmt.Sprintf("sum(rate(http_requests_total{job=\"search\"}[5m])) > %s",
		formatThreshold(starterMinRequestRate))
	if !strings.Contains(rule.Expr, guard) {
		t.Errorf("expression carries no minimum-traffic guard (want %q):\n%s", guard, rule.Expr)
	}
	if err := verifyStarterClassification(coverage.SignalErrors, rule); err != nil {
		t.Fatalf("the guarded expression no longer classifies as errors: %v", err)
	}
	// A guard that is not part of the rule is not a guard: the rendered
	// YAML must carry it too.
	in := StarterInput{
		Service: searchService(), Signal: coverage.SignalErrors,
		Measurement: m, Target: starterTarget,
	}
	p, refusal, err := BuildStarter(in)
	if err != nil || refusal != nil || p == nil {
		t.Fatalf("BuildStarter = (%v, %v, %v), want a proposal", p, refusal, err)
	}
	if err := p.Render(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.NewContent, guard) {
		t.Errorf("rendered rule does not carry the traffic guard:\n%s", p.NewContent)
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

// TestLatencyTemplateRejectsInfiniteP99 is the +Inf defect:
// histogram_quantile returns +Inf whenever the quantile falls in the `+Inf`
// bucket, and only NaN was guarded, so the threshold came out as
// `> 9223372036.8548` under a body stating "current p99 is 2562047h47m16s".
func TestLatencyTemplateRejectsInfiniteP99(t *testing.T) {
	for _, current := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		m := starterMeasurement{
			Supported: true, MetricUsed: "http_request_duration_seconds", Selector: `job="search"`,
			Current: current, Derived: true,
		}
		rule := latencyTemplate(searchService(), m)
		if rule.ThresholdDerived {
			t.Errorf("current=%v: ThresholdDerived = true; a non-finite reading is not a measurement", current)
		}
		want := "> " + formatThreshold(latencyFloor.Seconds())
		if !strings.HasSuffix(rule.Expr, want) {
			t.Errorf("current=%v: expr = %q, want it to fall back to the %s default", current, rule.Expr, want)
		}
		if !strings.Contains(rule.Explained, "fixed default") {
			t.Errorf("current=%v: explanation does not say the threshold is a default:\n%s", current, rule.Explained)
		}
	}
}

// TestQueryScalarSumDropsNonFiniteValues covers the same guard one level
// down, where the reading actually arrives.
func TestQueryScalarSumDropsNonFiniteValues(t *testing.T) {
	api := fakeInstantAPI{respond: func(string) (model.Value, error) {
		return model.Vector{{Metric: model.Metric{}, Value: model.SampleValue(math.Inf(1))}}, nil
	}}
	v, ok, err := queryScalarSum(context.Background(), api, "q", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("queryScalarSum = (%v, true), want ok == false for a +Inf reading", v)
	}
}

// TestLatencyTemplateCapsAPathologicalP99: the ceiling errorRatioCeiling
// applies for a ratio, applied to the other derived thresholds.
func TestLatencyTemplateCapsAPathologicalP99(t *testing.T) {
	m := starterMeasurement{
		Supported: true, MetricUsed: "http_request_duration_seconds", Selector: `job="search"`,
		Current: 600, Derived: true, // a ten-minute p99, measured mid-stall
	}
	rule := latencyTemplate(searchService(), m)
	if rule.ThresholdDerived {
		t.Error("ThresholdDerived = true for a capped threshold")
	}
	want := "> " + formatThreshold(latencyCeiling.Seconds())
	if !strings.HasSuffix(rule.Expr, want) {
		t.Errorf("expr = %q, want it capped at %s", rule.Expr, want)
	}
}

func TestSaturationTemplateCapsAPathologicalReading(t *testing.T) {
	m := starterMeasurement{
		Supported: true, MetricUsed: "process_resident_memory_bytes", Selector: `job="search"`,
		Current: 1e12, Derived: true,
	}
	rule := saturationTemplate(searchService(), m)
	if rule.ThresholdDerived {
		t.Error("ThresholdDerived = true for a capped threshold")
	}
	if !strings.HasSuffix(rule.Expr, "> "+formatThreshold(saturationCeiling)) {
		t.Errorf("expr = %q, want it capped at %v", rule.Expr, saturationCeiling)
	}
}

// --- naming conventions ----------------------------------------------------

// TestStarterTemplatesCoverRealNamingConventions is the Spring Boot / gRPC
// defect. verifyStarterClassification rejected every one of these, and that
// rejection was an error that aborted the entire run -- so one Micrometer
// service ended a fleet-wide propose pass, after earlier PRs had been
// opened.
func TestStarterTemplatesCoverRealNamingConventions(t *testing.T) {
	svc := coverage.Service{Name: "orders", Job: "orders", Source: "job", Up: true,
		Traffic: 12, TrafficBasis: coverage.BasisRequests}

	for _, metric := range durationHistogramMetrics {
		m := starterMeasurement{Supported: true, MetricUsed: metric, Selector: `job="orders"`, Current: 0.4, Derived: true}
		if err := verifyStarterClassification(coverage.SignalLatency, latencyTemplate(svc, m)); err != nil {
			t.Errorf("latency template for %s: %v", metric, err)
		}
	}
	for _, metric := range requestCounterMetrics {
		rateM := starterMeasurement{Supported: true, MetricUsed: metric, Selector: `job="orders"`}
		if err := verifyStarterClassification(coverage.SignalRate, rateTemplate(svc, rateM)); err != nil {
			t.Errorf("rate template for %s: %v", metric, err)
		}
		errM := starterMeasurement{
			Supported: true, MetricUsed: metric, Selector: `job="orders"`, CodeLabel: "code",
			Current: 0.01, Derived: true,
		}
		if err := verifyStarterClassification(coverage.SignalErrors, errorsTemplate(svc, errM)); err != nil {
			t.Errorf("errors template for %s: %v", metric, err)
		}
	}
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
// unreachable through RunStarters specifically BECAUSE RunStarters filters
// ExistingMatches to matched-scope only (matchedOnly) before calling
// BuildStarter: a genuine blind spot, by RankBlindSpots'/AnyScopedCoverage's
// definition, has zero MATCHED-scope matches on every signal.
//
// That claim used to be false, worded as "zero matches on every signal"
// with no mention of scope: RunStarters passed sc.Covered[sig] straight
// through unfiltered, so a genuine blind spot carrying an uncertain
// GLOBAL match (or, worse, a CERTAIN one -- see
// TestRunStartersProposesDespiteAGlobalCertainMatch) reached this branch
// for real, and a cluster-wide rule such as `up == 0` could turn into a
// wrong ReasonUncertainCoverage refusal, or silently swallow the proposal
// with none at all. BuildStarter's own contract is kept explicit and
// independently tested here regardless, the same way pr.Build documents
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
	m, err := probeRate(context.Background(), api, searchService(), testScanWindow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !m.Supported || m.MetricUsed != "http_requests_total" {
		t.Errorf("m = %+v, want Supported with http_requests_total", m)
	}
}

func TestProbeRateUnsupportedWhenNoRequestCounterExists(t *testing.T) {
	api := fakeInstantAPI{respond: func(string) (model.Value, error) { return model.Vector{}, nil }}
	m, err := probeRate(context.Background(), api, searchService(), testScanWindow, time.Now())
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

// thresholdFromExpr pulls the comparison threshold out of a starter
// expression's first `> N` -- the number a reviewer reads, extracted the
// same way rather than reconstructed from the template's own arithmetic.
func thresholdFromExpr(t *testing.T, expr string) float64 {
	t.Helper()
	i := strings.Index(expr, "> ")
	if i < 0 {
		t.Fatalf("no threshold comparison in %q", expr)
	}
	field := strings.Fields(expr[i+2:])[0]
	v, err := strconv.ParseFloat(field, 64)
	if err != nil {
		t.Fatalf("threshold %q in %q: %v", field, expr, err)
	}
	return v
}

// --- B4: absent() vs services that legitimately go quiet ------------------

// quietAPI answers probeRate's two queries: the metric exists, and the
// wentQuiet subquery reports whether it was ever absent.
func quietAPI(everAbsent bool) fakeInstantAPI {
	return fakeInstantAPI{respond: func(query string) (model.Value, error) {
		if strings.Contains(query, "max_over_time") {
			v := 0.0
			if everAbsent {
				v = 1
			}
			return model.Vector{{Metric: model.Metric{}, Value: model.SampleValue(v)}}, nil
		}
		if strings.Contains(query, "http_requests_total") {
			return vectorOf(model.Metric{"job": "search"}), nil
		}
		return model.Vector{}, nil
	}}
}

// TestProbeRateRefusesAServiceThatScalesToZero is the scale-to-zero defect.
// absent() over a request counter is propose's DEFAULT proposal (rate leads
// StarterPriority) at severity: page, so a KEDA-scaled or nightly-batch
// service was handed a rule that pages every night. Idle does not catch it:
// idle is a thirty-day average, and sixteen busy hours average out as busy.
func TestProbeRateRefusesAServiceThatScalesToZero(t *testing.T) {
	m, err := probeRate(context.Background(), quietAPI(true), searchService(), testScanWindow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !m.Supported {
		t.Fatalf("m = %+v, want the metric found", m)
	}
	if m.Refuse != ReasonIntermittentTraffic {
		t.Errorf("Refuse = %q, want ReasonIntermittentTraffic", m.Refuse)
	}

	p, refusal, err := BuildStarter(StarterInput{
		Service: searchService(), Signal: coverage.SignalRate, Measurement: m, Target: starterTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected no absent() proposal for a service whose traffic legitimately stops")
	}
	if refusal == nil || refusal.Reason != ReasonIntermittentTraffic {
		t.Errorf("refusal = %+v, want ReasonIntermittentTraffic", refusal)
	}
}

// TestProbeRateAcceptsContinuousTraffic is the other half: a service whose
// series never went away still gets the rate template.
func TestProbeRateAcceptsContinuousTraffic(t *testing.T) {
	m, err := probeRate(context.Background(), quietAPI(false), searchService(), testScanWindow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !m.Supported || m.Refuse != "" {
		t.Errorf("m = %+v, want Supported with no refusal", m)
	}
}

// TestProbeRateRefusesOnInsufficientHistory: a Prometheus younger than the
// quiet window has no `up` history to gate on, and absent() is equally true
// before a service was ever scraped -- so there is no evidence either way
// about whether this service legitimately goes quiet. That USED to be read
// as "not refused" (no evidence of scaling to zero taken as proof it
// doesn't), which is backwards for a page-severity starter rule: a probe
// that cannot see far enough back to be confident must refuse, not guess.
func TestProbeRateRefusesOnInsufficientHistory(t *testing.T) {
	api := fakeInstantAPI{respond: func(query string) (model.Value, error) {
		if strings.Contains(query, "max_over_time") {
			return model.Vector{}, nil // the `up` gate never opened
		}
		if strings.Contains(query, "http_requests_total") {
			return vectorOf(model.Metric{"job": "search"}), nil
		}
		return model.Vector{}, nil
	}}
	m, err := probeRate(context.Background(), api, searchService(), testScanWindow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Refuse != ReasonInsufficientHistory {
		t.Errorf("Refuse = %q, want ReasonInsufficientHistory: there was no history to conclude anything from", m.Refuse)
	}
}

// --- B1: one unrecognised service must not end the run --------------------

// TestBuildStarterRefusesRatherThanErroringOnAnUnverifiableTemplate pins the
// abort-the-whole-run defect at its source. A template that does not
// classify back as its own signal used to come out of BuildStarter as an
// ERROR, which RunStarters propagated -- ending the run after earlier
// services' PRs had already been opened, with nothing said about where it
// stopped.
func TestBuildStarterRefusesRatherThanErroringOnAnUnverifiableTemplate(t *testing.T) {
	// A saturation measurement whose metric coverage reads as errors: the
	// template builds, and verification catches the mismatch.
	in := StarterInput{
		Service: searchService(), Signal: coverage.SignalSaturation,
		Measurement: starterMeasurement{
			Supported: true, MetricUsed: "some_errors_total", Selector: `job="search"`,
			Current: 1e6, Derived: true,
		},
		Target: starterTarget,
	}
	p, refusal, err := BuildStarter(in)
	if err != nil {
		t.Fatalf("BuildStarter returned an error, want a refusal scoped to this service: %v", err)
	}
	if p != nil {
		t.Error("expected no proposal for a template that fails verification")
	}
	if refusal == nil || refusal.Reason != ReasonUnverifiable {
		t.Fatalf("refusal = %+v, want ReasonUnverifiable", refusal)
	}
	if refusal.Detail == "" {
		t.Error("refusal carries no detail; a verification failure must say what it saw")
	}
}

// TestRunStartersSurvivesAnUnsupportedService is the same defect one level
// up, end to end: a Micrometer service and a gRPC service alongside an
// ordinary one must all be reported, and the run must finish.
func TestRunStartersSurvivesAnUnsupportedService(t *testing.T) {
	// Each service exposes exactly one metric family, keyed by its job.
	metrics := map[string]string{
		"orders": "http_server_requests_seconds", // Micrometer: a latency histogram only
		"ledger": "grpc_server_handled_total",    // gRPC: a request counter only
		"search": "http_requests_total",
	}
	api := fakeInstantAPI{respond: func(query string) (model.Value, error) {
		for job, metric := range metrics {
			if !strings.Contains(query, fmt.Sprintf("job=%q", job)) {
				continue
			}
			if !strings.Contains(query, metric) {
				return model.Vector{}, nil
			}
			if strings.Contains(query, "max_over_time") {
				return model.Vector{{Metric: model.Metric{}, Value: 0}}, nil
			}
			return vectorOf(model.Metric{"job": model.LabelValue(job), "code": "200"}), nil
		}
		return model.Vector{}, nil
	}}

	var grid []coverage.ServiceCoverage
	for _, name := range []string{"orders", "ledger", "search"} {
		grid = append(grid, coverage.ServiceCoverage{
			Service: coverage.Service{
				Name: name, Job: name, Source: "job", Up: true,
				Traffic: 12, TrafficBasis: coverage.BasisRequests,
			},
			Covered: map[coverage.Signal][]coverage.RuleMatch{},
		})
	}

	cfg := config.Default()
	cfg.Rules.Path = "testdata"
	cfg.Coverage.RuleTargets = map[string]config.RuleTarget{}
	for _, name := range []string{"orders", "ledger", "search"} {
		cfg.Coverage.RuleTargets[name] = config.RuleTarget{File: starterTarget.File, Group: starterTarget.Group}
	}

	res, err := RunStarters(context.Background(), NewFakeProvider(), api, cfg,
		coverage.Result{Grid: grid}, time.Now(), RunOptions{})
	if err != nil {
		t.Fatalf("RunStarters aborted the whole run: %v", err)
	}

	proposed := map[string]bool{}
	for _, p := range res.Proposals {
		proposed[p.AlertName] = true
	}
	// Micrometer exposes only a latency histogram, so rate and errors find
	// no metric and latency is what lands. gRPC's handled_total is a
	// request counter, so rate lands. search is the ordinary case.
	for _, want := range []string{"OrdersLatencyHigh", "LedgerNoTraffic", "SearchNoTraffic"} {
		if !proposed[want] {
			t.Errorf("no proposal named %q; got %v (refusals: %v)", want, keysOf(proposed), res.Refusals)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRunStartersProposesDespiteAGlobalCertainMatch is N2: a cluster-wide,
// CERTAIN rule such as `up == 0` (Scope "global") must not suppress the
// rate starter for a genuine blind spot. Before matchedOnly filtered
// StarterInput.ExistingMatches, BuildStarter saw this global match, judged
// the service "genuinely covered" (any certain match, regardless of scope)
// and returned (nil, nil, nil) -- no proposal AND no refusal, so the
// service vanished from RunStarters' output entirely. Since `up == 0`
// exists in nearly every real Prometheus, this made the rate template dead
// fleet-wide.
func TestRunStartersProposesDespiteAGlobalCertainMatch(t *testing.T) {
	api := fakeInstantAPI{respond: func(query string) (model.Value, error) {
		if !strings.Contains(query, `job="search"`) {
			return model.Vector{}, nil
		}
		if strings.Contains(query, "max_over_time") {
			return model.Vector{{Metric: model.Metric{}, Value: 0}}, nil // never went quiet
		}
		if strings.Contains(query, "http_requests_total") {
			return vectorOf(model.Metric{"job": "search", "code": "200"}), nil
		}
		return model.Vector{}, nil
	}}

	grid := []coverage.ServiceCoverage{{
		Service: coverage.Service{
			Name: "search", Job: "search", Source: "job", Up: true,
			Traffic: 40.12, TrafficBasis: coverage.BasisRequests,
		},
		Covered: map[coverage.Signal][]coverage.RuleMatch{
			// A cluster-wide `up == 0`-shaped rule: certain, but global --
			// real coverage (the grid says so), but not specific to
			// "search", so it must not count as ExistingMatches here.
			coverage.SignalRate: {
				{GroupName: "org", AlertName: "TargetDown", Signal: coverage.SignalRate, Certain: true, Scope: "global"},
			},
		},
	}}

	cfg := config.Default()
	cfg.Rules.Path = "testdata"
	cfg.Coverage.RuleTargets = map[string]config.RuleTarget{
		"search": {File: starterTarget.File, Group: starterTarget.Group},
	}

	res, err := RunStarters(context.Background(), NewFakeProvider(), api, cfg,
		coverage.Result{Grid: grid}, time.Now(), RunOptions{})
	if err != nil {
		t.Fatalf("RunStarters: %v", err)
	}

	for _, r := range res.Refusals {
		if r.Group == "search" {
			t.Errorf("unexpected refusal for search: %+v (global TargetDown match must not block the rate starter)", r)
		}
	}
	found := false
	for _, p := range res.Proposals {
		if p.AlertName == "SearchNoTraffic" {
			found = true
		}
	}
	if !found {
		t.Errorf("no SearchNoTraffic proposal; got proposals=%v refusals=%v", res.Proposals, res.Refusals)
	}
}
