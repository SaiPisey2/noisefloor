package server

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/SaiPisey2/noisefloor/internal/scanner"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// metricsNamespace prefixes every series this package exports.
const metricsNamespace = "noisefloor"

// prometheusRegistry bundles the /metrics registry with the two vectors the
// HTTP middleware writes to directly, so server.go's instrument() does not
// need to reach through a registry lookup on every request.
type prometheusRegistry struct {
	registry     *prometheus.Registry
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

func newPrometheusRegistry(db *store.SQLite) *prometheusRegistry {
	reg := prometheus.NewRegistry()

	reg.MustRegister(&scanStatsCollector{db: db})

	httpRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace, Name: "http_requests_total",
		Help: "Total HTTP requests served, by method, route and status code.",
	}, []string{"method", "route", "code"})
	httpDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace, Name: "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, by method and route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
	reg.MustRegister(httpRequests, httpDuration)

	return &prometheusRegistry{registry: reg, httpRequests: httpRequests, httpDuration: httpDuration}
}

// scanStatsCollector reads scanner.ScanStats fresh from the store on every
// scrape, rather than caching it in memory: this process never runs a
// scan, so the only way it can be stale is by not re-reading the one row
// `noisefloor scan` last wrote. A collector (rather than a background
// goroutine updating gauges) also means a database that has never been
// scanned reports these series as simply absent, not zero -- a zero would
// read as "the last scan scored nothing", which is a different claim than
// "no scan has ever run".
type scanStatsCollector struct {
	db *store.SQLite
}

var (
	scanDurationDesc = prometheus.NewDesc(
		metricsNamespace+"_scan_duration_seconds",
		"Wall-clock duration of the most recently completed scan.", nil, nil)
	scanEpisodesDesc = prometheus.NewDesc(
		metricsNamespace+"_scan_episodes_reconstructed",
		"Episodes reconstructed by the most recently completed scan.", nil, nil)
	scanRulesScoredDesc = prometheus.NewDesc(
		metricsNamespace+"_scan_rules_scored",
		"Rules scored by the most recently completed scan.", nil, nil)
	scanTimestampDesc = prometheus.NewDesc(
		metricsNamespace+"_scan_last_success_timestamp_seconds",
		"Unix timestamp of the most recently completed scan.", nil, nil)
	scanFailuresDesc = prometheus.NewDesc(
		metricsNamespace+"_scan_query_failures",
		"Retryable Prometheus query failures absorbed during the most recently completed scan.", nil, nil)
	verdictDesc = prometheus.NewDesc(
		metricsNamespace+"_rules_by_verdict",
		"Number of scored rules by verdict, as of the most recently completed scan.",
		[]string{"verdict"}, nil)
)

func (c *scanStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- scanDurationDesc
	ch <- scanEpisodesDesc
	ch <- scanRulesScoredDesc
	ch <- scanTimestampDesc
	ch <- scanFailuresDesc
	ch <- verdictDesc
}

// allVerdicts is every verdict score.Verdict can return, so a verdict that
// currently has zero rules still reports 0 rather than being absent from
// the series -- a dashboard summing this metric must not need to know
// which verdicts exist.
var allVerdicts = []string{score.VerdictRetire, score.VerdictTune, score.VerdictAutomate, score.VerdictKeep}

func (c *scanStatsCollector) Collect(ch chan<- prometheus.Metric) {
	stats, ok, err := scanner.LoadStats(context.Background(), c.db)
	if err != nil || !ok {
		// No scan has ever completed against this database (or the stored
		// record is unreadable): report nothing rather than a misleading
		// zero for "no scan has run" versus "the last scan found nothing".
		return
	}
	ch <- prometheus.MustNewConstMetric(scanDurationDesc, prometheus.GaugeValue, stats.DurationSeconds)
	ch <- prometheus.MustNewConstMetric(scanEpisodesDesc, prometheus.GaugeValue, float64(stats.Episodes))
	ch <- prometheus.MustNewConstMetric(scanRulesScoredDesc, prometheus.GaugeValue, float64(stats.RulesScored))
	ch <- prometheus.MustNewConstMetric(scanTimestampDesc, prometheus.GaugeValue, float64(stats.ComputedAt.Unix()))
	ch <- prometheus.MustNewConstMetric(scanFailuresDesc, prometheus.GaugeValue, float64(stats.QueryFailures))
	for _, v := range allVerdicts {
		ch <- prometheus.MustNewConstMetric(verdictDesc, prometheus.GaugeValue, float64(stats.Verdicts[v]), v)
	}
}
