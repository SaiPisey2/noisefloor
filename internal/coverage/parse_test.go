package coverage

import (
	"reflect"
	"sort"
	"testing"
)

// TestExtract is the table of real-world rule expressions this project's
// task called for: expected metrics, matchers and (via classification)
// signal bucket, including Sloth- and Pyrra-shaped rules, a recording rule,
// a subquery, absent(), and one genuinely ambiguous expression.
func TestExtract(t *testing.T) {
	cases := []struct {
		name         string
		expr         string
		wantMetrics  []string
		wantMatcher  map[string][]string // checked as a subset: every key here must match exactly
		wantSubquery bool
		wantAbsent   []string
	}{
		{
			name:        "simple threshold",
			expr:        `process_resident_memory_bytes{service="checkout"} > 5e8`,
			wantMetrics: []string{"process_resident_memory_bytes"},
			wantMatcher: map[string][]string{"service": {"checkout"}},
		},
		{
			name: "error ratio, two selectors on the same metric",
			expr: `sum(rate(http_requests_total{service="checkout",code=~"5.."}[5m])) ` +
				`/ sum(rate(http_requests_total{service="checkout"}[5m])) > 0.05`,
			wantMetrics: []string{"http_requests_total"},
			wantMatcher: map[string][]string{"service": {"checkout"}, "code": {"5.."}},
		},
		{
			name:        "latency via histogram_quantile",
			expr:        `histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{service="checkout"}[5m])) by (le)) > 0.5`,
			wantMetrics: []string{"http_request_duration_seconds_bucket"},
			wantMatcher: map[string][]string{"service": {"checkout"}},
		},
		{
			name:        "absent traffic check",
			expr:        `absent(http_requests_total{service="checkout"})`,
			wantMetrics: []string{"http_requests_total"},
			wantMatcher: map[string][]string{"service": {"checkout"}},
			wantAbsent:  []string{"absent"},
		},
		{
			name:        "absent_over_time",
			expr:        `absent_over_time(up{job="search"}[10m])`,
			wantMetrics: []string{"up"},
			wantMatcher: map[string][]string{"job": {"search"}},
			wantAbsent:  []string{"absent_over_time"},
		},
		{
			name:        "recording rule reference (Pyrra-shaped)",
			expr:        `http_requests:burnrate5m{slo="checkout-availability"} > (14.4 * 0.001)`,
			wantMetrics: []string{"http_requests:burnrate5m"},
			wantMatcher: map[string][]string{"slo": {"checkout-availability"}},
		},
		{
			name:         "subquery",
			expr:         `max_over_time(deriv(node_load1[5m])[30m:1m]) > 0`,
			wantMetrics:  []string{"node_load1"},
			wantSubquery: true,
		},
		{
			name:        "mixed success/failure selector (genuinely ambiguous)",
			expr:        `sum(rate(http_requests_total{code=~"2..|5.."}[5m])) > 100`,
			wantMetrics: []string{"http_requests_total"},
			wantMatcher: map[string][]string{"code": {"2..|5.."}},
		},
		{
			name:        "aggregation by job, no explicit service matcher",
			expr:        `sum by (job) (rate(http_requests_total[5m])) == 0`,
			wantMetrics: []string{"http_requests_total"},
			wantMatcher: map[string][]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Extract(tc.expr)
			if err != nil {
				t.Fatalf("Extract(%q): %v", tc.expr, err)
			}
			if !reflect.DeepEqual(got.Metrics, tc.wantMetrics) {
				t.Errorf("Metrics = %v, want %v", got.Metrics, tc.wantMetrics)
			}
			for k, want := range tc.wantMatcher {
				var vals []string
				for _, m := range got.Matchers[k] {
					vals = append(vals, m.Value)
				}
				sort.Strings(vals)
				sort.Strings(want)
				if !reflect.DeepEqual(vals, want) {
					t.Errorf("Matchers[%q] = %v, want %v", k, vals, want)
				}
			}
			if got.HasSubquery != tc.wantSubquery {
				t.Errorf("HasSubquery = %v, want %v", got.HasSubquery, tc.wantSubquery)
			}
			if tc.wantAbsent != nil && !reflect.DeepEqual(got.AbsentFuncs, tc.wantAbsent) {
				t.Errorf("AbsentFuncs = %v, want %v", got.AbsentFuncs, tc.wantAbsent)
			}
		})
	}
}

// TestExtractPreservesNegativeMatchers pins the parse-level half of the
// `{code!~"2.."}` bug: negated matchers used to be dropped outright, so the
// one label saying what such a rule is about never reached classification.
func TestExtractPreservesNegativeMatchers(t *testing.T) {
	pe, err := Extract(`sum(rate(http_requests_total{job="search",code!~"2.."}[5m])) > 1`)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got := pe.Matchers["code"]
	if len(got) != 1 {
		t.Fatalf("Matchers[\"code\"] = %v, want exactly one matcher", got)
	}
	want := Matcher{Value: "2..", Negative: true, Regexp: true}
	if got[0] != want {
		t.Errorf("Matchers[\"code\"][0] = %+v, want %+v", got[0], want)
	}
	// A negated matcher mentions a label without naming a value the rule is
	// about, so it must not be visible to callers asking what the rule names.
	if vals, ok := pe.Positive("code"); ok {
		t.Errorf("Positive(\"code\") = %v, true; want no positive matcher", vals)
	}
	if vals, ok := pe.Positive("job"); !ok || len(vals) != 1 || vals[0] != "search" {
		t.Errorf("Positive(\"job\") = %v, %v; want [search], true", vals, ok)
	}
}

func TestMatcherMatches(t *testing.T) {
	cases := []struct {
		m      Matcher
		target string
		want   bool
	}{
		{Matcher{Value: "checkout"}, "checkout", true},
		{Matcher{Value: "checkout"}, "checkout-api", false},
		{Matcher{Value: "prod.*", Regexp: true}, "prod-eu", true},
		{Matcher{Value: "prod.*", Regexp: true}, "staging", false},
		// Prometheus anchors regexp matchers at both ends; a substring
		// match must not count.
		{Matcher{Value: "prod", Regexp: true}, "not-prod-really", false},
		{Matcher{Value: "checkout|billing", Regexp: true}, "billing", true},
		{Matcher{Value: "2..", Regexp: true, Negative: true}, "500", true},
		{Matcher{Value: "2..", Regexp: true, Negative: true}, "200", false},
	}
	for _, tc := range cases {
		if got := tc.m.Matches(tc.target); got != tc.want {
			t.Errorf("Matcher%+v.Matches(%q) = %v, want %v", tc.m, tc.target, got, tc.want)
		}
	}
}

func TestExtractHasAggregation(t *testing.T) {
	for expr, want := range map[string]bool{
		`up == 0`:                             false,
		`process_resident_memory_bytes > 5e8`: false,
		`sum by (job) (rate(http_requests_total[5m])) == 0`: true,
		`sum(rate(http_requests_total[5m])) > 100`:          true,
	} {
		pe, err := Extract(expr)
		if err != nil {
			t.Fatalf("Extract(%q): %v", expr, err)
		}
		if pe.HasAggregation != want {
			t.Errorf("Extract(%q).HasAggregation = %v, want %v", expr, pe.HasAggregation, want)
		}
	}
}

func TestExtractInvalidExpr(t *testing.T) {
	if _, err := Extract(`sum(rate(http_requests_total[5m])`); err == nil {
		t.Fatal("expected a parse error for an unbalanced expression, got nil")
	}
}

// TestClassify covers the signal-bucket table this project's task called
// for: rule expressions mapped to their expected bucket, including a
// Sloth-shaped rule, a Pyrra-shaped rule, and one genuinely ambiguous
// expression that must come back uncertain rather than confidently wrong.
func TestClassify(t *testing.T) {
	cases := []struct {
		name        string
		rule        Rule
		wantSignal  Signal
		wantCertain bool
		wantGen     string
		wantOK      bool
	}{
		{
			name: "generic error-rate ratio (certain: failure-only selector)",
			rule: Rule{AlertName: "CheckoutErrorRateHigh", Expr: `
				sum(rate(http_requests_total{service="checkout",code=~"5.."}[5m]))
				/ sum(rate(http_requests_total{service="checkout"}[5m])) > 0.05`},
			wantSignal: SignalErrors, wantCertain: true, wantOK: true,
		},
		{
			name:       "latency via histogram_quantile",
			rule:       Rule{AlertName: "CheckoutLatencyHigh", Expr: `histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{service="checkout"}[5m])) by (le)) > 0.5`},
			wantSignal: SignalLatency, wantCertain: true, wantOK: true,
		},
		{
			name:       "resource saturation",
			rule:       Rule{AlertName: "CheckoutMemoryHigh", Expr: `process_resident_memory_bytes{service="checkout"} > 5e8`},
			wantSignal: SignalSaturation, wantCertain: true, wantOK: true,
		},
		{
			name:       "absent() traffic check",
			rule:       Rule{AlertName: "CheckoutNoTraffic", Expr: `absent(http_requests_total{service="checkout"})`},
			wantSignal: SignalRate, wantCertain: true, wantOK: true,
		},
		{
			name:       "success-only selector reads as rate, not errors",
			rule:       Rule{AlertName: "TrafficDrop", Expr: `sum(rate(http_requests_total{code=~"2.."}[5m])) < 1`},
			wantSignal: SignalRate, wantCertain: true, wantOK: true,
		},
		{
			name:       "genuinely ambiguous: mixed success/failure selector",
			rule:       Rule{AlertName: "WeirdVolume", Expr: `sum(rate(http_requests_total{code=~"2..|5.."}[5m])) > 100`},
			wantSignal: SignalErrors, wantCertain: false, wantOK: true,
		},
		{
			name: "Sloth-generated burn-rate alert",
			rule: Rule{
				AlertName: "checkout-availability-page",
				Expr:      `slo:sli_error:ratio_rate5m{sloth_id="checkout-availability"} > (14.4 * 0.001)`,
				Labels: map[string]string{
					"sloth_id": "checkout-availability", "sloth_service": "checkout", "severity": "page",
				},
			},
			wantSignal: SignalBurnRate, wantCertain: true, wantGen: "sloth", wantOK: true,
		},
		{
			name: "Pyrra-generated burn-rate alert",
			rule: Rule{
				AlertName: "ErrorBudgetBurn",
				Expr:      `http_requests:burnrate5m{slo="checkout-availability"} > (14.4 * 0.001)`,
				Labels:    map[string]string{"slo": "checkout-availability", "severity": "page"},
			},
			wantSignal: SignalBurnRate, wantCertain: true, wantGen: "pyrra", wantOK: true,
		},
		{
			name:   "unrecognisable metric shape",
			rule:   Rule{AlertName: "Weird", Expr: `some_totally_novel_metric{} > 1`},
			wantOK: false,
		},
		{
			// An ordinary error-ratio rule built on a histogram's _count
			// series. "duration" in the name used to win outright and the
			// status selector was never consulted, so this read as LATENCY,
			// CERTAIN: coverage the service does not have, asserted, while
			// the error coverage it does have was denied.
			name: "error ratio over a duration histogram's _count series",
			rule: Rule{AlertName: "ErrorRatio", Expr: `
				sum(rate(http_request_duration_seconds_count{status=~"5.."}[5m]))
				/ sum(rate(http_request_duration_seconds_count[5m])) > 0.05`},
			wantSignal: SignalErrors, wantCertain: false, wantOK: true,
		},
		{
			// The other ordinary way to write an error rule: exclude the
			// successes instead of enumerating the failures. Dropping !~
			// matchers at parse time left this reading as plain traffic,
			// CERTAIN.
			name:       "error rule written by excluding success codes",
			rule:       Rule{AlertName: "NotOK", Expr: `sum(rate(http_requests_total{code!~"2.."}[5m])) > 1`},
			wantSignal: SignalErrors, wantCertain: true, wantOK: true,
		},
		{
			name:       "Micrometer request timer is latency",
			rule:       Rule{AlertName: "SpringLatency", Expr: `histogram_quantile(0.99, sum(rate(http_server_requests_seconds_bucket{job="orders"}[5m])) by (le)) > 2`},
			wantSignal: SignalLatency, wantCertain: true, wantOK: true,
		},
		{
			name:       "gRPC handling timer is latency",
			rule:       Rule{AlertName: "GRPCLatency", Expr: `histogram_quantile(0.99, sum(rate(grpc_server_handling_seconds_bucket{job="ledger"}[5m])) by (le)) > 2`},
			wantSignal: SignalLatency, wantCertain: true, wantOK: true,
		},
		{
			name: "gRPC completed-RPC counter is a request counter",
			rule: Rule{AlertName: "GRPCErrors", Expr: `
				sum(rate(grpc_server_handled_total{job="ledger",code=~"5.."}[5m]))
				/ sum(rate(grpc_server_handled_total{job="ledger"}[5m])) > 0.05`},
			wantSignal: SignalErrors, wantCertain: true, wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe, err := Extract(tc.rule.Expr)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			m, ok := Classify(tc.rule, pe)
			if ok != tc.wantOK {
				t.Fatalf("Classify ok = %v, want %v (match=%+v)", ok, tc.wantOK, m)
			}
			if !ok {
				return
			}
			if m.Signal != tc.wantSignal {
				t.Errorf("Signal = %q, want %q (reason: %s)", m.Signal, tc.wantSignal, m.Reason)
			}
			if m.Certain != tc.wantCertain {
				t.Errorf("Certain = %v, want %v (reason: %s)", m.Certain, tc.wantCertain, m.Reason)
			}
			if tc.wantGen != "" && m.Generator != tc.wantGen {
				t.Errorf("Generator = %q, want %q", m.Generator, tc.wantGen)
			}
			if m.Reason == "" {
				t.Error("Reason must never be empty -- every classification must be checkable")
			}
		})
	}
}
