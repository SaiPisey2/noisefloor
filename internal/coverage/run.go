package coverage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
)

// trafficStep is the sampling step used for both traffic-proxy range
// queries. Neither a request-counter's rate nor `scrape_samples_scraped`
// needs scan's per-minute precision -- this stays fixed and cheap over a
// multi-week window regardless of what prometheus.step is tuned to for
// ALERTS reconstruction.
const trafficStep = time.Hour

// RequestCounters are the request/operation counter conventions
// BuildTrafficProxies looks for, checked in this order and unioned
// together (requestRateQuery's `or` chain): the most common HTTP and gRPC
// server-side request-total metric names in Prometheus instrumentation
// today. Picked deliberately, not exhaustively -- extend this list, rather
// than adding a second mechanism, to recognise another one.
var RequestCounters = []string{
	"http_requests_total",        // the most common Go/Java/.NET HTTP server middleware convention
	"http_server_requests_total", // Micrometer / Spring Boot
	"grpc_server_handled_total",  // grpc-ecosystem/go-grpc-prometheus
	"requests_total",             // generic, framework-agnostic fallback name
}

// requestRateQuery returns one PromQL expression summing every recognised
// request counter's rate by service, unioned with `or` so a service is
// counted from whichever of RequestCounters it actually exposes. A service
// exposing more than one of these under the same job/namespace/service
// labels is undercounted (`or` keeps only the left-hand match for an
// overlapping label set) rather than double-counted -- the safer direction
// for a number that only ever decides a sort order, never a verdict.
func requestRateQuery() string {
	parts := make([]string, len(RequestCounters))
	for i, m := range RequestCounters {
		parts[i] = fmt.Sprintf("rate(%s[5m])", m)
	}
	return "sum by (job, namespace, service) (" + strings.Join(parts, " or ") + ")"
}

// Run performs one coverage pass: discover services from an instant `up`
// query and the traffic proxy, fetch every alerting rule Prometheus
// currently evaluates, and map them onto the discovered services.
//
// `up` is queried as of now, not averaged over the window -- whether a
// service currently exists is a present-tense question, unlike the traffic
// proxy, which is deliberately a window average (see BuildTrafficProxies).
func Run(ctx context.Context, api prom.Client, cfg config.Config, now time.Time) (result Result, err error) {
	upVal, err := api.Query(ctx, "up", now)
	if err != nil {
		return Result{}, fmt.Errorf("query up: %w", err)
	}
	upVec, ok := upVal.(model.Vector)
	if !ok {
		return Result{}, fmt.Errorf("query up: got %s, want vector", upVal.Type())
	}

	window := cfg.Window.Std()
	reqMatrix, err := api.QueryRange(ctx, requestRateQuery(), now.Add(-window), now, trafficStep)
	if err != nil {
		return Result{}, fmt.Errorf("query request-rate proxy: %w", err)
	}
	sampleMatrix, err := api.QueryRange(ctx, "scrape_samples_scraped", now.Add(-window), now, trafficStep)
	if err != nil {
		return Result{}, fmt.Errorf("query scrape_samples_scraped: %w", err)
	}
	proxies := BuildTrafficProxies(reqMatrix, sampleMatrix)

	disc := DiscoverServices(upVec, cfg.Coverage.Services, proxies, cfg.Coverage.ExcludeJobs)

	groups, err := api.Rules(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("fetch rules: %w", err)
	}
	var rules []Rule
	for _, g := range groups {
		for _, ar := range g.Alerting {
			rules = append(rules, Rule{
				GroupName: g.Name, AlertName: ar.Name, Expr: ar.Query, Labels: ar.Labels,
			})
		}
	}

	grid, parseErrors, unattributed := MapRules(rules, disc.Services)
	return Result{
		Grid: grid, ParseErrors: parseErrors,
		Unattributed: unattributed, ExcludedJobs: disc.ExcludedJobs,
	}, nil
}

// Result is Run's output: the coverage grid, any rules that failed to
// parse, any that parsed but named no discovered service, and which
// configured job exclusions actually took effect (see
// config.Coverage.ExcludeJobs) -- all carried through so Render can say
// what was left out rather than silently shrinking the report.
type Result struct {
	Grid        []ServiceCoverage
	ParseErrors map[string]error
	// Unattributed is every rule that parsed and classified but resolved to
	// no discovered service, keyed "group/alertname". See MapRules: without
	// this, such a rule and a genuine coverage gap are indistinguishable in
	// the grid, and the blind-spot list silently inherits the difference.
	Unattributed []string
	ExcludedJobs []string
}
