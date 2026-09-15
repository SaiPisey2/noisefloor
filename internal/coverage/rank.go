package coverage

import "sort"

// BlindSpot is one service with no alert coverage at all, ranked for
// display.
type BlindSpot struct {
	Service Service
	// Idle is true when this service's traffic proxy is negligible relative
	// to the busiest discovered service. See RankBlindSpots.
	Idle bool
}

// idleFraction is the traffic-proxy threshold, relative to the MEDIAN
// discovered service's traffic, below which a service is treated as idle
// rather than merely quiet. 5% comfortably separates a service exporting a
// handful of series (an idle heartbeat, nothing else) from one exporting
// the dozens a real HTTP service produces once it has a latency histogram
// and a few status/method label values.
//
// Deliberately the median, not the busiest service: Prometheus's own
// self-scrape (and, in a real cluster, node-exporter, cAdvisor, and any
// other infrastructure exporter) reports hundreds of samples regardless of
// business traffic, so it is routinely the single busiest target
// discovered. A threshold relative to THAT would make every genuinely busy
// application service read as idle by comparison -- verified against the
// demo stack, where scrape_samples_scraped{job="prometheus"} outweighs
// every application job by 20x or more. The median is robust to exactly
// one outlier in either direction; it takes several unusually large or
// small services to move it, which one self-monitoring job cannot do.
const idleFraction = 0.05

// RankBlindSpots returns every service with no alert coverage at all
// (ServiceCoverage.AnyCoverage() == false), ranked by the traffic proxy
// descending -- a service carrying real load with zero alerting sorts to
// the top, which is the headline case this package exists to surface.
//
// Idle services (traffic at or below idleFraction of the median discovered
// service's traffic) are appended after every non-idle blind spot and
// marked Idle: true, rather than merely left to sort last by their low
// traffic number: sorting last is a fact about relative order, "flagged as
// low-risk" is a claim about urgency, and only the second is what "an idle
// service is not a risk" actually requires a caller to be able to render.
func RankBlindSpots(grid []ServiceCoverage) []BlindSpot {
	traffic := make([]float64, len(grid))
	for i, sc := range grid {
		traffic[i] = sc.Service.Traffic
	}
	threshold := median(traffic) * idleFraction

	var active, idle []BlindSpot
	for _, sc := range grid {
		if sc.AnyCoverage() {
			continue
		}
		bs := BlindSpot{Service: sc.Service, Idle: sc.Service.Traffic <= threshold}
		if bs.Idle {
			idle = append(idle, bs)
		} else {
			active = append(active, bs)
		}
	}

	sort.SliceStable(active, func(i, j int) bool {
		if active[i].Service.Traffic != active[j].Service.Traffic {
			return active[i].Service.Traffic > active[j].Service.Traffic
		}
		return active[i].Service.Name < active[j].Service.Name
	})
	sort.SliceStable(idle, func(i, j int) bool { return idle[i].Service.Name < idle[j].Service.Name })

	return append(active, idle...)
}

// median returns the middle value of vs (averaging the two middle values
// for an even-length slice), without mutating vs.
func median(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vs...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
