package coverage

import "sort"

// BlindSpot is one service that no rule names specifically, ranked for
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

// sampleIdleFloor is an absolute floor applied within the BasisSamples
// group, in addition to idleFraction's relative check: a target exposing
// this few series or fewer is emitting essentially nothing beyond a bare
// liveness heartbeat, whatever that compares to among its peers.
//
// A bare sample count measures how much a target has to say for itself,
// and the honest range that spans is small enough (single digits to a few
// hundred) that a purely relative comparison between two already-tiny
// numbers can fail to trigger even when both are, in an absolute sense,
// negligible. Verified against the live demo: a batch worker exposing one
// gauge (1 sample) sits within the same order of magnitude as a
// lightly-instrumented sibling exposing a handful of gauges (8 samples),
// so 5% of their median alone never flagged it -- this floor does.
const sampleIdleFloor = 2

// requestIdleFloor is the same absolute floor for the BasisRequests group,
// in requests per second.
//
// An earlier version of this file asserted that a measured req/s figure
// "needs no such floor" because it is already an absolute, meaningful rate.
// It never implemented one, and the relative test alone cannot stand in for
// it, for a reason that has nothing to do with the unit: a median is a
// comparison against peers, and it degenerates whenever the peers do not
// vary. With exactly ONE blind spot the median IS that service's own
// traffic, so the test reads "is x <= 0.05x" and no single blind spot is
// ever idle. With every blind spot at the same value -- a dev cluster
// ticking over at half a request per second each -- the median is again
// that value and nothing is idle, so every service in it earns a
// page-severity starter proposal on day one.
//
// One request per second is the line: below it, the five-minute windows a
// starter rule evaluates over hold a few hundred requests at most, so a
// single failed request moves an error ratio by a whole percentage point.
// That is the regime where a brand-new, page-severity rule measures noise
// rather than load, which is exactly what Idle exists to keep it away from.
const requestIdleFloor = 1.0

// RankBlindSpots returns every service that no rule names specifically
// (ServiceCoverage.AnyScopedCoverage() == false), ranked so a service
// carrying real load with zero alerting of its own sorts near the top --
// the headline case this package exists to surface. See
// AnyScopedCoverage for why a global-scope match does not clear a service
// off this list even though the grid credits it.
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
		if sc.AnyScopedCoverage() {
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
// idle services after every non-idle one in the group. A service is idle
// when it is negligible relative to this group's own median (idleFraction)
// or below the group's absolute floor (absoluteIdleFloor) -- either test
// alone leaves a hole the other covers.
func rankWithinBasis(basis string, group []ServiceCoverage) []BlindSpot {
	traffic := make([]float64, len(group))
	for i, sc := range group {
		traffic[i] = sc.Service.Traffic
	}
	threshold := median(traffic) * idleFraction

	var active, idle []BlindSpot
	for _, sc := range group {
		bs := BlindSpot{
			Service: sc.Service,
			Idle:    sc.Service.Traffic <= threshold || sc.Service.Traffic <= absoluteIdleFloor(basis),
		}
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

// absoluteIdleFloor is the traffic value at or below which a service is
// idle outright, whatever its peers read -- the check idleFraction's median
// cannot make (see requestIdleFloor). A basis with no absolute meaning
// (the empty basis: no traffic data was available at all) gets no floor,
// since there is no measurement to compare against one.
func absoluteIdleFloor(basis string) float64 {
	switch basis {
	case BasisRequests:
		return requestIdleFloor
	case BasisSamples:
		return sampleIdleFloor
	default:
		return -1 // below any traffic value, including zero: never triggers
	}
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
