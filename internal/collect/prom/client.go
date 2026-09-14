// Package prom wraps the Prometheus HTTP API with only the calls noisefloor
// needs, so the rest of the codebase does not depend on the upstream client.
package prom

import (
	"context"
	"errors"
	"fmt"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/SaiPisey2/noisefloor/internal/config"
)

var ErrEmptyResult = errors.New("prometheus returned no data")

type AlertingRule struct {
	Name        string
	Query       string
	For         time.Duration
	Labels      map[string]string
	Annotations map[string]string
	Health      string
}

type RuleGroup struct {
	Name     string
	File     string
	Alerting []AlertingRule
}

type Client interface {
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (model.Matrix, error)
	Rules(ctx context.Context) ([]RuleGroup, error)
	RetentionFloor(ctx context.Context) (time.Time, error)
}

type API struct{ v1 v1.API }

func New(cfg config.Prometheus) (*API, error) {
	c, err := promapi.NewClient(promapi.Config{Address: cfg.URL})
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
		// Warnings are not fatal but callers deserve to see them once.
		fmt.Printf("prometheus warning: %v\n", warnings)
	}
	m, ok := val.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("query_range %q returned %s, want matrix", query, val.Type())
	}
	return m, nil
}

func (a *API) Rules(ctx context.Context) ([]RuleGroup, error) {
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
				Health:      string(ar.Health),
			})
		}
		out = append(out, rg)
	}
	return out, nil
}

// probePoints is how many samples the retention probe asks for. 720 keeps a
// 30-day probe at one point per hour, which is ample to locate the first hour
// that has any ALERTS data at all.
const probePoints = 720

// RetentionFloor reports the oldest timestamp within [from, to) for which
// Prometheus can still answer questions about ALERTS, so reports never imply
// more history than actually exists.
//
// This MUST be a range query. An instant query of `min(timestamp(ALERTS))`
// looks only at series present within the lookback window, so it returns
// approximately now regardless of how much history exists — which would clamp
// every backfill window to zero length and score nothing at all.
//
// It deliberately measures the oldest ALERTS data rather than the oldest data
// in the TSDB. `prometheus_tsdb_lowest_timestamp_seconds` would be cheaper but
// only exists when Prometheus scrapes itself, which is not true of agent-mode,
// Thanos, Mimir or most managed deployments.
func (a *API) RetentionFloor(ctx context.Context, from, to time.Time) (time.Time, error) {
	if !to.After(from) {
		return time.Time{}, fmt.Errorf("retention floor: window %s..%s is empty",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	step := to.Sub(from) / probePoints
	if step < time.Minute {
		step = time.Minute
	}

	m, err := a.QueryRange(ctx, `count(ALERTS)`, from, to, step)
	if err != nil {
		return time.Time{}, fmt.Errorf("retention floor probe: %w", err)
	}

	earliest := time.Time{}
	for _, series := range m {
		for _, v := range series.Values {
			t := v.Timestamp.Time().UTC()
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
	}
	if earliest.IsZero() {
		return time.Time{}, ErrEmptyResult
	}
	return earliest, nil
}

func labelSetToMap(ls model.LabelSet) map[string]string {
	m := make(map[string]string, len(ls))
	for k, v := range ls {
		m[string(k)] = string(v)
	}
	return m
}
