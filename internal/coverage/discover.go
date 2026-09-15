package coverage

import (
	"sort"

	"github.com/prometheus/common/model"
)

// Traffic-proxy bases. See TrafficProxy and BuildTrafficProxies: a service
// gets BasisRequests whenever it exposes a recognised request/operation
// counter, and only falls back to BasisSamples (a measurement of how much a
// target has to say for itself, not of how much work it does) when it
// exposes none. The two are not comparable numbers -- a rank must never mix
// them silently, which is why every Service that has a traffic value also
// carries the basis it was measured in, and RankBlindSpots ranks within one
// basis at a time (see its doc comment).
const (
	BasisRequests = "req/s"
	BasisSamples  = "scrape samples"
)

// TrafficProxy is one service's traffic-proxy reading: a value together
// with the unit it was measured in.
type TrafficProxy struct {
	Value float64
	Basis string
}

// KeyFor returns the discovery key DiscoverServices derives for a set of
// labels: (namespace, service) wins over job whenever both are present. A
// caller building a traffic map from a differently-labelled query (see
// SumTraffic) uses this so the two maps agree on what identifies a service.
func KeyFor(namespace, service, job string) string {
	if namespace != "" && service != "" {
		return "k8s:" + namespace + "/" + service
	}
	return "job:" + job
}

// DiscoveryResult is DiscoverServices' output: the discovered services, plus
// which configured exclusions actually matched something -- so a caller can
// say what was left out rather than silently shrinking the list. See
// config.Coverage.ExcludeJobs.
type DiscoveryResult struct {
	Services []Service
	// ExcludedJobs is every job name from excludeJobs that at least one
	// `up` sample actually carried, in the order first seen. A configured
	// exclusion that matched nothing is not included -- there is nothing
	// to report having left out.
	ExcludedJobs []string
}

// DiscoverServices builds the service set from an instant `up` vector plus
// an optional explicit list, and attaches each one's traffic-proxy reading
// from proxyByKey (see BuildTrafficProxies). See this package's doc comment
// for what a "service" is and why (namespace, service) outranks job
// whenever both are present on a target's `up` sample.
//
// excludeJobs names job values to drop from discovery entirely, before the
// namespace/service grouping is even applied -- see config.Coverage.
// ExcludeJobs's doc comment for why "prometheus" is excluded by default.
func DiscoverServices(up model.Vector, explicit []string, proxyByKey map[string]TrafficProxy, excludeJobs []string) DiscoveryResult {
	excluded := make(map[string]bool, len(excludeJobs))
	for _, j := range excludeJobs {
		excluded[j] = true
	}

	byKey := map[string]*Service{}
	var order []string
	seenExcluded := map[string]bool{}
	var excludedOut []string

	for _, s := range up {
		job := string(s.Metric["job"])
		ns := string(s.Metric["namespace"])
		svc := string(s.Metric["service"])
		if job == "" && (ns == "" || svc == "") {
			continue // nothing to key a service on
		}
		if excluded[job] {
			if !seenExcluded[job] {
				seenExcluded[job] = true
				excludedOut = append(excludedOut, job)
			}
			continue
		}

		key := KeyFor(ns, svc, job)
		source := "job"
		name := job
		if ns != "" && svc != "" {
			source = "k8s"
			name = ns + "/" + svc
		}

		if existing, ok := byKey[key]; ok {
			if s.Value > 0 {
				existing.Up = true
			}
			continue
		}
		proxy := proxyByKey[key]
		byKey[key] = &Service{
			Name: name, Namespace: ns, Service: svc, Job: job,
			Source: source, Up: s.Value > 0,
			Traffic: proxy.Value, TrafficBasis: proxy.Basis,
		}
		order = append(order, key)
	}

	for _, name := range explicit {
		if hasName(byKey, order, name) {
			continue
		}
		key := "config:" + name
		if _, ok := byKey[key]; ok {
			continue
		}
		byKey[key] = &Service{Name: name, Job: name, Source: "config"}
		order = append(order, key)
	}

	out := make([]Service, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return DiscoveryResult{Services: out, ExcludedJobs: excludedOut}
}

func hasName(byKey map[string]*Service, order []string, name string) bool {
	for _, k := range order {
		if byKey[k].Name == name {
			return true
		}
	}
	return false
}

// SumTraffic computes the per-service average of a range-queried matrix,
// keyed the way KeyFor groups a service (see BuildTrafficProxies for how
// this is used for both the request-rate and the scrape-sample-volume
// queries): each series' value is averaged over the window, then summed
// across every series mapping to the same service key, so a service backed
// by several targets sums their averages rather than only keeping one.
func SumTraffic(m model.Matrix) map[string]float64 {
	out := map[string]float64{}
	for _, series := range m {
		if len(series.Values) == 0 {
			continue
		}
		job := string(series.Metric["job"])
		ns := string(series.Metric["namespace"])
		svc := string(series.Metric["service"])
		key := KeyFor(ns, svc, job)

		var sum float64
		for _, v := range series.Values {
			sum += float64(v.Value)
		}
		out[key] += sum / float64(len(series.Values))
	}
	return out
}

// BuildTrafficProxies combines the two traffic signals into one map, per
// service key: a service that exposes a recognised request/operation
// counter (reqRate, already summed by service and rated -- see
// requestRateQuery) is measured in BasisRequests; a service that exposes
// none of them falls back to BasisSamples, from a range query of
// `scrape_samples_scraped` (sampleVolume). See this package's doc comment
// for why request throughput is preferred and sample volume is only the
// fallback -- and why the two must never be compared directly.
func BuildTrafficProxies(reqRate, sampleVolume model.Matrix) map[string]TrafficProxy {
	out := map[string]TrafficProxy{}
	for key, v := range SumTraffic(sampleVolume) {
		out[key] = TrafficProxy{Value: v, Basis: BasisSamples}
	}
	for key, v := range SumTraffic(reqRate) {
		out[key] = TrafficProxy{Value: v, Basis: BasisRequests} // overrides the sample-volume fallback
	}
	return out
}
