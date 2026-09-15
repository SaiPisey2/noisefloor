package coverage

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/common/model"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
)

// trafficStep is the sampling step used for the traffic-proxy range query.
// scrape_samples_scraped only changes when a target's own metric surface
// changes shape (a deploy, a restart, a new endpoint) -- it needs nothing
// like scan's per-minute precision, so this stays fixed and cheap over a
// multi-week window regardless of what prometheus.step is tuned to for
// ALERTS reconstruction.
const trafficStep = time.Hour

// Run performs one coverage pass: discover services from an instant `up`
// query and the traffic proxy, fetch every alerting rule Prometheus
// currently evaluates, and map them onto the discovered services.
//
// `up` is queried as of now, not averaged over the window -- whether a
// service currently exists is a present-tense question, unlike the traffic
// proxy, which is deliberately a window average (see SumTraffic).
func Run(ctx context.Context, api prom.Client, cfg config.Config, now time.Time) (grid []ServiceCoverage, parseErrors map[string]error, err error) {
	upVal, err := api.Query(ctx, "up", now)
	if err != nil {
		return nil, nil, fmt.Errorf("query up: %w", err)
	}
	upVec, ok := upVal.(model.Vector)
	if !ok {
		return nil, nil, fmt.Errorf("query up: got %s, want vector", upVal.Type())
	}

	window := cfg.Window.Std()
	trafficMatrix, err := api.QueryRange(ctx, "scrape_samples_scraped", now.Add(-window), now, trafficStep)
	if err != nil {
		return nil, nil, fmt.Errorf("query scrape_samples_scraped: %w", err)
	}

	services := DiscoverServices(upVec, cfg.Coverage.Services, SumTraffic(trafficMatrix))

	groups, err := api.Rules(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch rules: %w", err)
	}
	var rules []Rule
	for _, g := range groups {
		for _, ar := range g.Alerting {
			rules = append(rules, Rule{
				GroupName: g.Name, AlertName: ar.Name, Expr: ar.Query, Labels: ar.Labels,
			})
		}
	}

	grid, parseErrors = MapRules(rules, services)
	return grid, parseErrors, nil
}
