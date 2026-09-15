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

// idleFraction is the traffic-proxy threshold, relative to the busiest
// discovered service, below which a service is treated as idle rather than
// merely quiet. 5% comfortably separates a service exporting a handful of
// series (an idle heartbeat, nothing else) from one exporting the dozens a
// real HTTP service produces once it has a latency histogram and a few
// status/method label values -- without hard-coding an absolute sample
// count that would only be right for one particular instrumentation style.
const idleFraction = 0.05

// RankBlindSpots returns every service with no alert coverage at all
// (ServiceCoverage.AnyCoverage() == false), ranked by the traffic proxy
// descending -- a service carrying real load with zero alerting sorts to
// the top, which is the headline case this package exists to surface.
//
// Idle services (traffic at or below idleFraction of the busiest service)
// are appended after every non-idle blind spot and marked Idle: true,
// rather than merely left to sort last by their low traffic number: sorting
// last is a fact about relative order, "flagged as low-risk" is a claim
// about urgency, and only the second is what "an idle service is not a
// risk" actually requires a caller to be able to render.
func RankBlindSpots(grid []ServiceCoverage) []BlindSpot {
	var maxTraffic float64
	for _, sc := range grid {
		if sc.Service.Traffic > maxTraffic {
			maxTraffic = sc.Service.Traffic
		}
	}
	threshold := maxTraffic * idleFraction

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
