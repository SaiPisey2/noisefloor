package coverage

import "sort"

// BlindSpot is one service with no alert coverage at all, ranked for
// display.
type BlindSpot struct {
	Service Service
	// Idle is true when this service's traffic proxy is negligible relative
	// to the median service measured in the SAME basis. See RankBlindSpots.
	Idle bool
}

// idleFraction is the traffic-proxy threshold, relative to the MEDIAN
// traffic within a basis group (see RankBlindSpots), below which a service
// is treated as idle rather than merely quiet. 5% comfortably separates a
// service exporting a handful of series, or serving a trickle of requests,
// from one doing real work.
//
// Deliberately the median, not the busiest service in the group: a single
// unusually large or small service cannot move it, where it could move a
// max-relative threshold outright. This is what caught a real bug during
// this package's own development: with request throughput not yet
// distinguished from sample volume, Prometheus's own self-scrape (hundreds
// of samples regardless of business traffic) was the single busiest target
// discovered, and a threshold relative to IT made every genuinely busy
// application service read as idle by comparison. Grouping by basis (see
// RankBlindSpots) was the real fix for that particular case -- Prometheus's
// self-scrape and an application's request rate are different bases and no
// longer compared at all -- but the median is kept as well, since the same
// kind of outlier (a chatty infrastructure exporter, an unusually quiet
// service) can occur within a single basis too.
const idleFraction = 0.05

// sampleIdleFloor is an absolute floor applied only within the BasisSamples
// group, in addition to idleFraction's relative check: a target exposing
// this few series or fewer is emitting essentially nothing beyond a bare
// liveness heartbeat, whatever that compares to among its peers.
//
// BasisRequests needs no such floor -- a measured requests/second figure is
// already an absolute, meaningful rate on its own (a fraction of a request
// per second is negligible regardless of what else was discovered). A bare
// sample count is not: it measures how much a target has to say for
// itself, and the honest range that spans is small enough (single digits
// to a few hundred) that a purely relative comparison between two already-
// tiny numbers can fail to trigger even when both are, in an absolute
// sense, negligible. Verified against the live demo: a batch worker
// exposing one gauge (1 sample) sits within the same order of magnitude as
// a lightly-instrumented sibling exposing a handful of gauges (8 samples),
// so 5% of their median alone never flagged it -- this floor does.
const sampleIdleFloor = 2

// RankBlindSpots returns every service with no alert coverage at all
// (ServiceCoverage.AnyCoverage() == false), ranked so a service carrying
// real load with zero alerting sorts near the top -- the headline case
// this package exists to surface.
//
// Traffic is ranked WITHIN one basis at a time, never across bases: a
// service measured in requests/second and one measured in scrape-sample
// count are different units, and sorting them against each other directly
// would silently assert a conversion rate between "how much work a service
// does" and "how many metrics it happens to expose" that does not exist.
// Every BasisRequests service (real, measured throughput -- the more
// trustworthy signal) is therefore ranked internally and listed before
// every BasisSamples service (the fallback for a service with no
// recognised request counter), which is in turn ranked internally. A
// caller rendering this list must show each row's basis (Service.
// TrafficBasis) rather than only the number, so nobody reads a
// sample-volume count as if it were a rate.
//
// Idle services (traffic at or below idleFraction of their own basis
// group's median) are appended after every non-idle blind spot IN THAT
// GROUP and marked Idle: true, rather than merely left to sort last by
// their low number: sorting last is a fact about relative order, "flagged
// as low-risk" is a claim about urgency, and only the second is what "an
// idle service is not a risk" actually requires a caller to be able to
// render.
func RankBlindSpots(grid []ServiceCoverage) []BlindSpot {
	byBasis := map[string][]ServiceCoverage{}
	for _, sc := range grid {
		if sc.AnyCoverage() {
			continue
		}
		byBasis[sc.Service.TrafficBasis] = append(byBasis[sc.Service.TrafficBasis], sc)
	}

	var out []BlindSpot
	for _, basis := range basisOrder(byBasis) {
		out = append(out, rankWithinBasis(basis, byBasis[basis])...)
	}
	return out
}

// basisOrder returns the bases present, in display order: BasisRequests
// (real, measured throughput) first, then BasisSamples, then anything else
// -- in practice only "", a service with no traffic data at all -- sorted
// for determinism.
func basisOrder(present map[string][]ServiceCoverage) []string {
	var order []string
	for _, b := range []string{BasisRequests, BasisSamples} {
		if _, ok := present[b]; ok {
			order = append(order, b)
		}
	}
	var rest []string
	for b := range present {
		if b != BasisRequests && b != BasisSamples {
			rest = append(rest, b)
		}
	}
	sort.Strings(rest)
	return append(order, rest...)
}

// rankWithinBasis ranks one basis group by traffic descending, appending
// idle services (see idleFraction, computed against this group's own
// median, plus sampleIdleFloor for a BasisSamples group) after every
// non-idle one in the group.
func rankWithinBasis(basis string, group []ServiceCoverage) []BlindSpot {
	traffic := make([]float64, len(group))
	for i, sc := range group {
		traffic[i] = sc.Service.Traffic
	}
	threshold := median(traffic) * idleFraction

	var active, idle []BlindSpot
	for _, sc := range group {
		isIdle := sc.Service.Traffic <= threshold
		if basis == BasisSamples && sc.Service.Traffic <= sampleIdleFloor {
			isIdle = true
		}
		bs := BlindSpot{Service: sc.Service, Idle: isIdle}
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
