// Package coverage finds services that carry no alerting at all -- the
// inverse of what internal/score does. score finds rules that page for
// nothing; coverage finds the silence on the other side, which the research
// this project started from put at 78% of organisations having suffered an
// incident where no alert fired (see docs/noisefloor-design.md).
//
// # What a "service" is
//
// job and namespace/service disagree in real clusters -- a single workload
// can expose several scrape jobs (an app port and a sidecar's metrics port),
// and a single job can span several Kubernetes services (a shared exporter).
// Neither is "the" right unit; the choice determines every row this package
// reports, so it is made explicitly and up front:
//
// A Service is the smallest thing a team would say "we own this" about. In a
// cluster with Kubernetes service-discovery labels, that is (namespace,
// service): it groups every job/pod belonging to one workload, so a sidecar
// or a metrics-only port does not appear as its own, separately-blind
// service next to the workload it belongs to. Outside Kubernetes, or wherever
// those SD labels are absent, `job` is the only grouping Prometheus offers at
// all, so it stands in directly -- one job is treated as one service. An
// explicit list in config overrides both, for a target whose `up` labels
// don't reflect how the team actually refers to it (a federated
// pull-through, a relabeled job name).
//
// # Traffic proxy
//
// Ranking blind spots needs a notion of "how much does this service
// matter", and the honest answer has two tiers, not one.
//
// The good case: a service that exposes a recognised request/operation
// counter (RequestCounters -- http_requests_total and similar) is measured
// by its actual summed rate, in requests per second. That is a real
// throughput number and the preferred basis whenever it exists.
//
// Not every workload has one -- a batch job, a database, a cache have no
// http_requests_total to point at. For those, this package falls back to a
// sample count: Prometheus records `scrape_samples_scraped` for every
// target on every scrape, regardless of what it exports, and a service
// under real load and real instrumentation (multiple endpoints, methods,
// status codes, a latency histogram) produces far more of it than an idle
// or minimally-instrumented one. It is deliberately not a measurement of
// request rate -- an early version of this package used it as the ONLY
// proxy and it ranked Prometheus's own self-scrape (hundreds of internal
// metrics) above every real application service, which is a unit error,
// not a finding.
//
// The two bases are not comparable numbers, so every traffic reading
// carries which one it is (Service.TrafficBasis) and RankBlindSpots ranks
// within one basis at a time rather than ever sorting them against each
// other directly. See BuildTrafficProxies and RankBlindSpots.
package coverage

// Signal is one of the five things a rule can be alerting on, for the
// purpose of this package's coverage grid.
type Signal string

const (
	// SignalRate covers traffic: is the service receiving/processing
	// requests at all, including "traffic disappeared" (absent()) checks.
	SignalRate Signal = "rate"
	// SignalErrors covers failure ratio or failure count alerting.
	SignalErrors Signal = "errors"
	// SignalLatency covers request/operation duration.
	SignalLatency Signal = "latency"
	// SignalSaturation covers resource utilisation: CPU, memory, disk,
	// queue depth, file descriptors, connection pools.
	SignalSaturation Signal = "saturation"
	// SignalBurnRate covers SLO error-budget burn-rate alerting, generated
	// by tools such as Sloth or Pyrra rather than hand-written.
	SignalBurnRate Signal = "burn_rate"
)

// Signals is every bucket the coverage grid has a column for, in display
// order.
var Signals = []Signal{SignalRate, SignalErrors, SignalLatency, SignalSaturation, SignalBurnRate}

// Service is one coverage unit. See this file's package doc for the
// definition and why it was chosen.
type Service struct {
	// Name is how the service is displayed and how rules are matched to it:
	// "<namespace>/<service>" when discovered via Kubernetes SD labels,
	// otherwise the bare job name.
	Name      string
	Namespace string
	Service   string // the k8s SD "service" label; empty when Source == "job" or "config"
	Job       string
	// Source records which discovery path found this service: "k8s" (from
	// namespace+service SD labels on `up`), "job" (from `up`'s job label
	// alone, no SD labels present), or "config" (named explicitly in
	// config, not observed in `up` at all -- Up is always false for these).
	Source string
	// Up is whether the most recently discovered `up` sample for this
	// service read 1. A service present in config but never scraped, or
	// scraped and currently down, both read false here; Source tells them
	// apart.
	Up bool
	// Traffic is the raw traffic-proxy value (see package doc and
	// BuildTrafficProxies): a real throughput reading when this service
	// exposes a recognised request/operation counter, else a fallback
	// sample-volume reading. Only meaningful relative to another Traffic
	// value with the SAME TrafficBasis -- it is not a rate on its own, and
	// req/s and scrape-sample counts must never be compared directly.
	Traffic float64
	// TrafficBasis is the unit Traffic was measured in: BasisRequests or
	// BasisSamples. Empty when no traffic data was available at all (e.g.
	// a Service named only via explicit config, never observed in `up`).
	TrafficBasis string
}

// RuleMatch is one rule attributed to one service for one signal.
type RuleMatch struct {
	GroupName string
	AlertName string
	Signal    Signal
	// Certain is false when the signal bucket is a heuristic guess rather
	// than a confident read -- see Classify. A caller must render this
	// distinction rather than assert the guess as fact.
	Certain bool
	// Reason is a short, human-readable justification for the
	// classification, always populated (not only when Certain is false) so
	// the grid can explain a confident call too, not just an uncertain one.
	Reason string
	// Generator names the SLO rule generator this rule's label conventions
	// matched ("sloth", "pyrra"), or "" for a hand-written rule. See
	// Classify's doc comment for exactly which conventions are checked.
	Generator string
	// Scope is "matched" when the rule's own label matchers named this
	// service specifically (job/namespace/service equality or a regexp that
	// resolves to it), or "global" when the rule carries no such matcher at
	// all and was attributed to every discovered service because it
	// aggregates broadly (e.g. `sum by (job) (...)` with no job matcher).
	// A global match is real but less specific, and the report says so
	// rather than presenting it identically to an explicit one.
	Scope string
}

// ServiceCoverage is one row of the coverage grid: a service and, per
// signal, every rule matched onto it (nil/empty when uncovered).
type ServiceCoverage struct {
	Service Service
	Covered map[Signal][]RuleMatch
}

// Covers reports whether at least one rule covers sig for this service.
func (sc ServiceCoverage) Covers(sig Signal) bool {
	return len(sc.Covered[sig]) > 0
}

// AnyCoverage reports whether this service has any alert coverage at all,
// on any signal, counting a global-scope match the same as an explicit one.
// This is what the grid renders.
func (sc ServiceCoverage) AnyCoverage() bool {
	for _, sig := range Signals {
		if sc.Covers(sig) {
			return true
		}
	}
	return false
}

// AnyScopedCoverage reports whether any rule names this service
// specifically (RuleMatch.Scope "matched"), as opposed to sweeping it up
// with every other service in the cluster.
//
// This, not AnyCoverage, is what blind-spot ranking asks (see
// RankBlindSpots). A cluster-wide `up == 0` is real alerting and the grid
// says so, but it is one rule that fires identically for every target
// noisefloor discovered; it tells a team nothing about whether anyone is
// watching THEIR service. Letting it satisfy "has alerting" would mean a
// single global rule anywhere in a Prometheus emptied the blind-spot list
// for the entire estate -- turning the one honest fix for global
// attribution (see resolveTargets) into a way to hide every gap it exists
// to surface.
//
// It also requires Certain: a "matched" scope only says a rule's own label
// matchers name this service; it says nothing about whether Classify was
// SURE what signal that rule is about. Before this required Certain too, a
// guessed classification (Certain: false -- a heuristic read, not a
// confident one, see Classify) cleared a service off the blind-spot list
// exactly as if it were confirmed, and propose's own ReasonUncertainCoverage
// refusal existed specifically because certain=false is not "covered" --
// AnyScopedCoverage disagreeing with that everywhere else it is asked meant
// a guess could hide a blind spot from the report entirely, with no refusal
// and no row to say why. A tool that knows it is guessing must not let that
// guess stand in for knowledge -- so a service whose only scoped match on a
// signal is uncertain is still a blind spot: it surfaces here, and reaches
// propose's own explicit ReasonUncertainCoverage refusal instead of
// vanishing silently.
func (sc ServiceCoverage) AnyScopedCoverage() bool {
	for _, sig := range Signals {
		for _, m := range sc.Covered[sig] {
			if m.Scope == "matched" && m.Certain {
				return true
			}
		}
	}
	return false
}

// Rule is the subset of a rule definition coverage needs: prom.AlertingRule
// carries the rest (For, Annotations) but coverage only ever reads these.
// Declared locally, rather than importing internal/collect/prom, so this
// package has no dependency on how rules are fetched.
type Rule struct {
	GroupName string
	AlertName string
	Expr      string
	Labels    map[string]string
}
