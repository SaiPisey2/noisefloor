// This file extends faultgen with several synthetic services carrying
// deliberately different alert coverage, so `noisefloor coverage` (issue
// #10) has something real to find in the demo stack. Each is scraped under
// its own Prometheus job (see demo/prometheus/prometheus.yml) and exposes
// its own metrics path here, using realistic metric names
// (http_requests_total, http_request_duration_seconds_bucket,
// process_resident_memory_bytes) rather than invented ones -- the point is
// to exercise recognition of real-world shapes, not shapes made up for the
// occasion. Matching rules live in demo/prometheus/rules/coverage.yml.
//
//	checkout    -- fully covered: traffic, errors, latency and saturation
//	               alerts all exist. Scraped with namespace=shop,
//	               service=checkout Kubernetes-SD-style labels.
//	billing     -- partially covered: a saturation alert only, despite
//	               exposing the same full metric surface as checkout --
//	               the most common real shape (a team notices resource
//	               pressure and alerts on it, and never gets round to
//	               error rate or latency). Also namespace=shop.
//	search      -- the busiest endpoint here (most method/status-code
//	               label combinations, and correspondingly the highest
//	               scrape_samples_scraped -- see internal/coverage's
//	               traffic-proxy doc comment), and NO rule mentions it at
//	               all. The headline blind spot. No SD labels: discovered
//	               by job alone, exercising that fallback path.
//	batchworker -- exposes almost nothing (one gauge, no request metric
//	               at all): no traffic and no alerts. Coverage must not
//	               rank this as an urgent blind spot -- an idle service
//	               alerting on nothing is not a risk. No SD labels.
package main

import (
	"fmt"
	"math"
	"net/http"
)

// httpBuckets is a fixed, made-up cumulative latency shape used to build
// every synthetic histogram below -- for exercising rule expressions
// (histogram_quantile, rate-of-bucket), not a claim about real latency
// distributions.
var httpBuckets = []struct {
	le   string
	frac float64
}{
	{"0.05", 0.55}, {"0.1", 0.80}, {"0.25", 0.92}, {"0.5", 0.97}, {"1", 0.99}, {"2.5", 1.0},
}

// writeCounter emits one Prometheus counter sample. total is the counter's
// current value; callers derive it from elapsed() (declared in main.go),
// which only grows, so it is monotonic for the life of the process exactly
// like a real counter.
func writeCounter(w http.ResponseWriter, name, labels string, total float64) {
	fmt.Fprintf(w, "%s{%s} %g\n", name, labels, total)
}

// writeHistogram emits a synthetic cumulative histogram for one label set:
// the standard bucket lines, the +Inf bucket, _sum and _count.
func writeHistogram(w http.ResponseWriter, name, labels string, total float64) {
	for _, b := range httpBuckets {
		fmt.Fprintf(w, "%s_bucket{%s,le=%q} %g\n", name, labels, b.le, total*b.frac)
	}
	fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %g\n", name, labels, total)
	fmt.Fprintf(w, "%s_sum{%s} %g\n", name, labels, total*0.15)
	fmt.Fprintf(w, "%s_count{%s} %g\n", name, labels, total)
}

func checkoutMetrics(w http.ResponseWriter, _ *http.Request) {
	t := elapsed()
	get, post := t*6, t*2 // ~8 req/s combined
	getErr, postErr := get*0.02, post*0.02
	mem := 4e8 + 1.5e8*math.Sin(t/600) // oscillates across CheckoutMemoryHigh's 5e8 threshold

	fmt.Fprintln(w, "# TYPE http_requests_total counter")
	writeCounter(w, "http_requests_total", `method="GET",code="200"`, get-getErr)
	writeCounter(w, "http_requests_total", `method="GET",code="500"`, getErr)
	writeCounter(w, "http_requests_total", `method="POST",code="200"`, post-postErr)
	writeCounter(w, "http_requests_total", `method="POST",code="500"`, postErr)

	fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")
	writeHistogram(w, "http_request_duration_seconds", `method="GET"`, get)
	writeHistogram(w, "http_request_duration_seconds", `method="POST"`, post)

	fmt.Fprintln(w, "# TYPE process_resident_memory_bytes gauge")
	fmt.Fprintf(w, "process_resident_memory_bytes %g\n", mem)
}

// billingMetrics exposes the same full metric surface as checkout -- traffic,
// errors and latency are all measurable -- but demo/prometheus/rules/
// coverage.yml only alerts on its saturation. The gap is a choice the rule
// author made, not a limit of what the service reports, which is the
// realistic shape partial coverage actually takes.
func billingMetrics(w http.ResponseWriter, _ *http.Request) {
	t := elapsed()
	get, post := t*4, t*1.5 // ~5.5 req/s combined
	getErr, postErr := get*0.01, post*0.01
	mem := 3.5e8 + 1.6e8*math.Sin(t/540)

	fmt.Fprintln(w, "# TYPE http_requests_total counter")
	writeCounter(w, "http_requests_total", `method="GET",code="200"`, get-getErr)
	writeCounter(w, "http_requests_total", `method="GET",code="500"`, getErr)
	writeCounter(w, "http_requests_total", `method="POST",code="200"`, post-postErr)
	writeCounter(w, "http_requests_total", `method="POST",code="500"`, postErr)

	fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")
	writeHistogram(w, "http_request_duration_seconds", `method="GET"`, get)
	writeHistogram(w, "http_request_duration_seconds", `method="POST"`, post)

	fmt.Fprintln(w, "# TYPE process_resident_memory_bytes gauge")
	fmt.Fprintf(w, "process_resident_memory_bytes %g\n", mem)
}

// searchMetrics is the busiest, highest-cardinality endpoint in the fixture
// -- more methods, more status codes, higher request volume -- and no rule
// in demo/prometheus/rules ever mentions it. That combination is the
// headline blind spot noisefloor coverage exists to find.
func searchMetrics(w http.ResponseWriter, _ *http.Request) {
	t := elapsed()
	total := t * 40 // ~40 req/s -- substantial traffic
	codes := []struct {
		code string
		frac float64
	}{{"200", 0.90}, {"301", 0.03}, {"400", 0.03}, {"404", 0.03}, {"500", 0.01}}
	methods := []struct {
		name string
		frac float64
	}{{"GET", 0.7}, {"POST", 0.3}}

	fmt.Fprintln(w, "# TYPE http_requests_total counter")
	for _, m := range methods {
		for _, c := range codes {
			writeCounter(w, "http_requests_total",
				fmt.Sprintf("method=%q,code=%q", m.name, c.code), total*m.frac*c.frac)
		}
	}

	fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")
	for _, m := range methods {
		writeHistogram(w, "http_request_duration_seconds", fmt.Sprintf("method=%q", m.name), total*m.frac)
	}

	fmt.Fprintln(w, "# TYPE process_resident_memory_bytes gauge")
	fmt.Fprintf(w, "process_resident_memory_bytes %g\n", 5e8+1e8*math.Sin(t/700))
}

// batchworkerMetrics is deliberately almost nothing: no request counter at
// all, because this is not a request-driven service, and no alert rule
// mentions it either. Its traffic-proxy value (scrape_samples_scraped)
// stays near zero because there is almost nothing here to scrape, which is
// what correctly keeps it out of the urgent blind-spot ranking -- see
// internal/coverage.RankBlindSpots's idleFraction.
func batchworkerMetrics(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "# TYPE process_resident_memory_bytes gauge")
	fmt.Fprintf(w, "process_resident_memory_bytes %g\n", 5e7)
}
