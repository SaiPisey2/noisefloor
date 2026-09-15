package coverage

import "sort"

// MapRules attributes every rule to the services its expression names,
// classifying each attribution by signal (Classify) and building the
// coverage grid: one ServiceCoverage per discovered service, in the same
// order as services.
//
// A rule whose expression carries an explicit job/namespace/service matcher
// is attributed only to the services that matcher resolves to. A rule with
// no such matcher anywhere, but that aggregates by one of those labels
// (`sum by (job) (...)`), is attributed to every discovered service with
// RuleMatch.Scope "global": it evaluates once per group in Prometheus, so
// from the outside it covers all of them -- just less specifically than a
// rule that names one directly, which the report says explicitly rather
// than presenting the two identically.
//
// Rules that fail to parse are returned in parseErrors, keyed by
// "group/alertname", rather than silently dropped: a coverage report that
// stays quiet about a rule it could not read is indistinguishable from one
// that read it and found nothing, which is the one failure mode this
// package exists to avoid.
//
// unattributed is the other half of that same guarantee, and the half that
// was missing: every rule that parsed AND classified cleanly but resolved
// to zero discovered services, keyed the same way. Counting only unparsable
// rules left a real coverage gap and a rule this package simply could not
// place looking identical from the outside -- both render as an empty cell.
// A caller must report these, so "N rules could not be attributed to any
// service" is visible rather than silently inflating the blind-spot list.
func MapRules(rules []Rule, services []Service) (grid []ServiceCoverage, parseErrors map[string]error, unattributed []string) {
	parseErrors = map[string]error{}
	covered := make(map[string]map[Signal][]RuleMatch, len(services))
	for _, s := range services {
		covered[s.Name] = map[Signal][]RuleMatch{}
	}

	for _, r := range rules {
		pe, err := Extract(r.Expr)
		if err != nil {
			parseErrors[r.GroupName+"/"+r.AlertName] = err
			continue
		}
		match, ok := Classify(r, pe)
		if !ok {
			continue // no recognisable metric shape; nothing to attribute
		}

		targets, scope := resolveTargets(pe, services)
		match.Scope = scope
		if len(targets) == 0 {
			unattributed = append(unattributed, r.GroupName+"/"+r.AlertName)
			continue
		}
		for _, name := range targets {
			covered[name][match.Signal] = append(covered[name][match.Signal], match)
		}
	}

	grid = make([]ServiceCoverage, len(services))
	for i, s := range services {
		grid[i] = ServiceCoverage{Service: s, Covered: covered[s.Name]}
	}
	sort.Strings(unattributed)
	return grid, parseErrors, unattributed
}

// resolveTargets says which services' names a rule's parsed expression
// names, and whether that attribution was explicit ("matched") or inferred
// from the expression spanning every service ("global"). Precedence mirrors
// how specifically a selector names a service: an explicit "service"
// matcher (optionally narrowed by "namespace") beats a bare "namespace"
// matcher (every service in that namespace), which beats a bare "job"
// matcher, which beats falling back to grouping.
//
// Each strategy is tried in turn and a strategy that resolves to NO
// discovered service falls through to the next rather than ending the
// search. That fall-through is the whole point: a rule carrying
// `job="kube-state-metrics"` names the exporter that exposes the series,
// not the workload the rule is about, and stopping at the first scoping
// label present meant every such rule was credited to nobody -- which this
// package then reported as its subject having no alerting. Under-
// attribution here is not the safe direction (see matcherFor): it
// manufactures blind spots that do not exist, which is precisely the
// confident wrong answer this feature exists to avoid.
//
// A rule that falls through every strategy is attributed to nothing and
// reported as unattributable (see MapRules), rather than being silently
// indistinguishable from a real gap.
func resolveTargets(pe ParsedExpr, services []Service) (names []string, scope string) {
	svc, hasSvc := matcherFor(pe, "service")
	ns, hasNS := matcherFor(pe, "namespace")
	job, hasJob := matcherFor(pe, "job")

	if hasSvc {
		for _, s := range services {
			if !svc.Matches(s.Service) {
				continue
			}
			if hasNS && !ns.Matches(s.Namespace) {
				continue
			}
			names = append(names, s.Name)
		}
		if len(names) > 0 {
			return names, "matched"
		}
	}
	if hasNS {
		for _, s := range services {
			if ns.Matches(s.Namespace) {
				names = append(names, s.Name)
			}
		}
		if len(names) > 0 {
			return names, "matched"
		}
	}
	if hasJob {
		for _, s := range services {
			if job.Matches(s.Job) {
				names = append(names, s.Name)
			}
		}
		if len(names) > 0 {
			return names, "matched"
		}
	}

	// scoped, here, means "narrowed by SOME label matcher", not only by
	// job/namespace/service: hasSvc/hasNS/hasJob alone under-counts it. See
	// spansEveryService's doc comment for why any matcher at all -- even one
	// this package cannot resolve to a discovered service -- must block the
	// global inference.
	scoped := hasSvc || hasNS || hasJob || len(pe.Matchers) > 0
	if !spansEveryService(pe, scoped, hasSvc || hasNS || hasJob) {
		return nil, "matched"
	}
	for _, s := range services {
		names = append(names, s.Name)
	}
	return names, "global"
}

// spansEveryService reports whether the expression evaluates across every
// service rather than narrowing to specific job/namespace/service values.
//
// Two shapes qualify. The first is broad aggregation: `by (job)` groups
// per job, and `without (le)` does too, since `without` keeps every label
// except the ones named, so job/namespace/service survive unless one of
// them is explicitly dropped.
//
// The second is no aggregation at all. `up == 0` names no service, drops
// no label and produces one alert instance per target: from the outside it
// covers every service discovered here, and reading it as covering none
// (which is what treating "no grouping" as "not broad" did) turns the most
// common global rule in existence into a cluster-wide false blind spot.
// That inference holds only while nothing narrowed the expression, which
// scoped says: a rule reading `{service="ghost"}` with no aggregation
// evaluates over exactly the series that selector matches, and if those
// belong to no discovered service then the honest answer is "no service",
// not "all of them".
//
// scoped must mean "carries ANY equality or regexp matcher beyond the
// metric name", not only a job/namespace/service one -- resolveTargets
// computes it that way, as len(pe.Matchers) > 0 rather than
// hasSvc||hasNS||hasJob alone. A rule such as
// `node_filesystem_avail_bytes{mountpoint="/",instance="node1"} < 1e9`
// narrows to one filesystem on one node: it is scoped to SOMETHING, even
// though this package has no idea what service that something belongs to,
// and crediting it as `global` (as the narrower job/namespace/service-only
// check did) marks every discovered service as covered by a rule that in
// truth covers none of them. The honest answer for a matcher this package
// cannot resolve is the same as for `{service="ghost"}` above: unattributed,
// not "all of them".
func spansEveryService(pe ParsedExpr, scoped, namesScope bool) bool {
	if !pe.HasAggregation {
		return !scoped
	}
	if pe.GroupingWithout {
		for _, g := range pe.Grouping {
			if g == "job" || g == "namespace" || g == "service" {
				return false
			}
		}
		// `without` only RETAINS the scoping labels, where `by (namespace)`
		// positively asserts one result per namespace. That difference
		// matters once a positive matcher has already pinned
		// job/namespace/service to a value resolving to no discovered
		// service: nothing fans out, and the rule covers exactly the one
		// unresolved thing it names. `min without (alertmanager) (
		// ...{job="prometheus"})` in the Prometheus mixin was read as
		// cluster-wide on this branch and credited error coverage to every
		// service in the fleet, while its four unaggregated siblings --
		// same selector, no wrapper -- were correctly left unattributed.
		return !namesScope
	}
	for _, g := range pe.Grouping {
		if g == "job" || g == "namespace" || g == "service" {
			return true
		}
	}
	return false
}

// matcherFor returns the first POSITIVE matcher on label, and whether there
// was one. Only positive matchers are consulted, because only they name a
// service: `job!="prometheus"` mentions the job label without saying
// anything about which job the rule is about, and treating it as a name
// would attribute the rule to the one service it explicitly excludes.
//
// Matching is a real, anchored regexp evaluation (Matcher.Matches), not a
// literal-alternation approximation. An earlier version compared only
// "|"-separated literals and matched nothing at all against a genuinely
// dynamic pattern such as `namespace=~"prod.*"`, on the stated grounds that
// under-attribution was the safe direction. For this feature it is the
// dangerous one: every service in every prod namespace was reported as
// having no coverage from a rule that covers all of them.
func matcherFor(pe ParsedExpr, label string) (Matcher, bool) {
	for _, m := range pe.Matchers[label] {
		if !m.Negative {
			return m, true
		}
	}
	return Matcher{}, false
}
