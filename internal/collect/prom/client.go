// Package prom wraps the Prometheus HTTP API with only the calls noisefloor
// needs, so the rest of the codebase does not depend on the upstream client.
package prom

import (
	"context"
	"fmt"
	"os"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/SaiPisey2/noisefloor/internal/config"
)

type AlertingRule struct {
	Name        string
	Query       string
	For         time.Duration
	Labels      map[string]string
	Annotations map[string]string
}

type RuleGroup struct {
	Name     string
	File     string
	Alerting []AlertingRule
}

type Client interface {
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (model.Matrix, error)
	// Query runs an instant query at ts. internal/coverage uses it for `up`,
	// where the question is "does this exist right now", not a range.
	Query(ctx context.Context, query string, ts time.Time) (model.Value, error)
	Rules(ctx context.Context) ([]RuleGroup, error)
}

type API struct{ v1 v1.API }

// An interface nothing asserts against is an interface that silently rots.
var _ Client = (*API)(nil)

func New(cfg config.Prometheus) (*API, error) {
	rt, err := cfg.Auth.Transport()
	if err != nil {
		return nil, fmt.Errorf("prometheus client: %w", err)
	}
	c, err := promapi.NewClient(promapi.Config{Address: cfg.URL, RoundTripper: rt})
	if err != nil {
		return nil, fmt.Errorf("prometheus client: %w", err)
	}
	return &API{v1: v1.NewAPI(c)}, nil
}

func (a *API) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (model.Matrix, error) {
	val, warnings, err := a.v1.QueryRange(ctx, query, v1.Range{Start: start, End: end, Step: step})
	if err != nil {
		return nil, fmt.Errorf("query_range %q [%s..%s]: %w",
			query, start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	if len(warnings) > 0 {
		// Warnings are not fatal, but they go to stderr, never stdout: the CLI
		// prints its report to stdout and a warning interleaved into it would
		// corrupt the output for anyone piping or parsing it.
		fmt.Fprintf(os.Stderr, "prometheus warning: %v\n", warnings)
	}
	m, ok := val.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("query_range %q returned %s, want matrix", query, val.Type())
	}
	return m, nil
}

func (a *API) Query(ctx context.Context, query string, ts time.Time) (model.Value, error) {
	val, warnings, err := a.v1.Query(ctx, query, ts)
	if err != nil {
		return nil, fmt.Errorf("query %q @%s: %w", query, ts.Format(time.RFC3339), err)
	}
	if len(warnings) > 0 {
		// See QueryRange's identical comment: warnings go to stderr, never
		// stdout, so they cannot corrupt a report someone is piping.
		fmt.Fprintf(os.Stderr, "prometheus warning: %v\n", warnings)
	}
	return val, nil
}

func (a *API) Rules(ctx context.Context) ([]RuleGroup, error) {
	// client_golang v1.24+ added a matcher-set argument to filter rule groups
	// by series selector; nil means "no filter, return all rule groups",
	// which is what we want here.
	res, err := a.v1.Rules(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch rules: %w", err)
	}
	var out []RuleGroup
	for _, g := range res.Groups {
		rg := RuleGroup{Name: g.Name, File: g.File}
		for _, r := range g.Rules {
			ar, ok := r.(v1.AlertingRule)
			if !ok {
				continue // recording rules are not scoreable
			}
			rg.Alerting = append(rg.Alerting, AlertingRule{
				Name:        ar.Name,
				Query:       ar.Query,
				For:         time.Duration(ar.Duration) * time.Second,
				Labels:      labelSetToMap(ar.Labels),
				Annotations: labelSetToMap(ar.Annotations),
			})
		}
		out = append(out, rg)
	}
	return out, nil
}

func labelSetToMap(ls model.LabelSet) map[string]string {
	m := make(map[string]string, len(ls))
	for k, v := range ls {
		m[string(k)] = string(v)
	}
	return m
}
