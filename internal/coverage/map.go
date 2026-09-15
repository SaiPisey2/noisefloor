package coverage

import "strings"

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
func MapRules(rules []Rule, services []Service) (grid []ServiceCoverage, parseErrors map[string]error) {
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
		for _, name := range targets {
			covered[name][match.Signal] = append(covered[name][match.Signal], match)
		}
	}

	grid = make([]ServiceCoverage, len(services))
	for i, s := range services {
		grid[i] = ServiceCoverage{Service: s, Covered: covered[s.Name]}
	}
	return grid, parseErrors
}

// resolveTargets says which services' names a rule's parsed expression
// names, and whether that attribution was explicit ("matched") or inferred
// from broad aggregation ("global"). Precedence mirrors how specifically a
// selector names a service: an explicit "service" matcher (optionally
// narrowed by "namespace") beats a bare "namespace" matcher (every service
// in that namespace), which beats a bare "job" matcher, which beats falling
// back to aggregation grouping.
func resolveTargets(pe ParsedExpr, services []Service) (names []string, scope string) {
	ns, hasNS := firstValue(pe.Matchers, "namespace")
	svc, hasSvc := firstValue(pe.Matchers, "service")
	job, hasJob := firstValue(pe.Matchers, "job")

	switch {
	case hasSvc:
		for _, s := range services {
			if !matchesValue(svc, s.Service) {
				continue
			}
			if hasNS && !matchesValue(ns, s.Namespace) {
				continue
			}
			names = append(names, s.Name)
		}
		return names, "matched"
	case hasNS:
		for _, s := range services {
			if matchesValue(ns, s.Namespace) {
				names = append(names, s.Name)
			}
		}
		return names, "matched"
	case hasJob:
		for _, s := range services {
			if matchesValue(job, s.Job) {
				names = append(names, s.Name)
			}
		}
		return names, "matched"
	}

	if !hasBroadGrouping(pe) {
		return nil, "matched"
	}
	for _, s := range services {
		names = append(names, s.Name)
	}
	return names, "global"
}

// hasBroadGrouping reports whether the expression aggregates in a way that
// spans every service rather than narrowing to specific job/namespace/
// service values -- `by (job)` does; `without (le)` does too, since
// `without` keeps every label except the ones named, so job/namespace/
// service survive unless one of them is explicitly dropped.
func hasBroadGrouping(pe ParsedExpr) bool {
	if pe.GroupingWithout {
		for _, g := range pe.Grouping {
			if g == "job" || g == "namespace" || g == "service" {
				return false
			}
		}
		return true
	}
	for _, g := range pe.Grouping {
		if g == "job" || g == "namespace" || g == "service" {
			return true
		}
	}
	return false
}

func firstValue(m map[string][]string, key string) (string, bool) {
	vs, ok := m[key]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// matchesValue reports whether a matcher's value (a plain string, or a
// "|"-separated regexp alternation such as "checkout|billing") names
// target exactly. This is not a real regexp evaluation -- it only resolves
// the common "name one of these literal values" shape rule generators and
// hand-written rules both produce; a genuinely dynamic pattern (`.*`,
// character classes) will not match anything here, which fails toward
// under- rather than over-attribution.
func matchesValue(pattern, target string) bool {
	if target == "" {
		return false
	}
	for _, alt := range strings.Split(pattern, "|") {
		alt = strings.TrimPrefix(alt, "^")
		alt = strings.TrimSuffix(alt, "$")
		if alt == target {
			return true
		}
	}
	return false
}
