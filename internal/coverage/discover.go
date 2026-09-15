package coverage

import (
	"sort"

	"github.com/prometheus/common/model"
)

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

// DiscoverServices builds the service set from an instant `up` vector plus
// an optional explicit list, and attaches each one's traffic-proxy value
// from trafficByKey (see SumTraffic). See this package's doc comment for
// what a "service" is and why (namespace, service) outranks job whenever
// both are present on a target's `up` sample.
func DiscoverServices(up model.Vector, explicit []string, trafficByKey map[string]float64) []Service {
	byKey := map[string]*Service{}
	var order []string

	for _, s := range up {
		job := string(s.Metric["job"])
		ns := string(s.Metric["namespace"])
		svc := string(s.Metric["service"])
		if job == "" && (ns == "" || svc == "") {
			continue // nothing to key a service on
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
		byKey[key] = &Service{
			Name: name, Namespace: ns, Service: svc, Job: job,
			Source: source, Up: s.Value > 0, Traffic: trafficByKey[key],
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
	return out
}

func hasName(byKey map[string]*Service, order []string, name string) bool {
	for _, k := range order {
		if byKey[k].Name == name {
			return true
		}
	}
	return false
}

// SumTraffic computes the traffic-proxy value per service key (see KeyFor)
// from a range-queried matrix of `scrape_samples_scraped`: each series'
// value is averaged over the window, then summed across every series
// mapping to the same service key (a service backed by several targets
// sums their averages, rather than only keeping one). See this package's
// doc comment for why `scrape_samples_scraped` is the proxy.
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
