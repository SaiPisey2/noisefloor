package coverage

import (
	"fmt"
	"strings"
)

// Classify buckets one rule into a Signal from its own labels (for SLO
// generator detection) and its parsed expression (for everything else). The
// returned RuleMatch always carries GroupName/AlertName/Signal/Certain/
// Reason/Generator; MapRules fills in Scope once it knows which services the
// rule was attributed to. ok is false only when the expression referenced no
// metric shape this package recognises at all -- that is reported as
// "unclassifiable", never silently coerced into a signal.
//
// Signal classification is inherently heuristic. This function is honest
// about it in two ways: every RuleMatch carries a Reason so a human can
// check the call, and Certain is false whenever the evidence genuinely
// supports more than one reading -- most importantly, a bare request/
// operation counter (http_requests_total and friends) is ambiguous between
// "traffic volume" and "error rate" until its own selectors say which,
// exactly the case docs/noisefloor-design.md calls out. A confidently wrong
// "you have no error alerting" aimed at a team that does have one is worse
// than saying nothing.
func Classify(r Rule, pe ParsedExpr) (RuleMatch, bool) {
	m := RuleMatch{GroupName: r.GroupName, AlertName: r.AlertName}

	if gen, evidence := detectGenerator(r, pe); gen != "" {
		m.Signal, m.Certain, m.Generator = SignalBurnRate, true, gen
		m.Reason = fmt.Sprintf("detected as %s-generated via %s", gen, evidence)
		return m, true
	}

	type guess struct {
		sig     Signal
		certain bool
		reason  string
	}
	var guesses []guess

	for _, metric := range pe.Metrics {
		sig, certain, reason := classifyMetricName(metric)
		if sig == "" {
			continue
		}
		if sig == SignalRate {
			// The one shape genuinely ambiguous by name alone: a generic
			// request/operation counter could be tracking volume or
			// failures depending entirely on which values its own
			// selectors pin a status/code-shaped label to.
			if s, c, why, ok := classifyRequestCounterBySelector(pe); ok {
				sig, certain, reason = s, c, why
			}
		}
		guesses = append(guesses, guess{sig, certain, reason})
	}

	if len(pe.AbsentFuncs) > 0 {
		guesses = append(guesses, guess{SignalRate, true,
			fmt.Sprintf("uses %s() -- checking for the total absence of a series is a "+
				"traffic/availability check, not a threshold on its value", strings.Join(pe.AbsentFuncs, ", "))})
	}

	if len(guesses) == 0 {
		return m, false
	}

	first := guesses[0]
	allSame := true
	for _, g := range guesses[1:] {
		if g.sig != first.sig {
			allSame = false
			break
		}
	}

	if allSame {
		m.Signal, m.Certain, m.Reason = first.sig, first.certain, first.reason
		return m, true
	}

	// Metrics of more than one apparent kind in the same expression -- do
	// not silently pick one and assert it. This is reported, not hidden.
	kinds := make([]string, 0, len(guesses))
	seen := map[Signal]bool{}
	for _, g := range guesses {
		if !seen[g.sig] {
			seen[g.sig] = true
			kinds = append(kinds, string(g.sig))
		}
	}
	m.Signal = first.sig
	m.Certain = false
	m.Reason = fmt.Sprintf("expression references metrics suggesting more than one signal (%s); "+
		"classification is a guess", strings.Join(kinds, ", "))
	return m, true
}

// classifyMetricName guesses a signal purely from a metric's name. Never
// used alone to call a rule "certain" for the one ambiguous shape
// (request/operation counters) -- see classifyRequestCounterBySelector.
func classifyMetricName(name string) (sig Signal, certain bool, reason string) {
	lower := strings.ToLower(name)
	switch {
	case lower == "up":
		return SignalRate, true, "\"up\" is the standard Prometheus target-availability metric"
	case strings.Contains(lower, "error"):
		return SignalErrors, true, fmt.Sprintf("metric name %q contains \"error\"", name)
	case strings.Contains(lower, "latency") || strings.Contains(lower, "duration"):
		return SignalLatency, true, fmt.Sprintf("metric name %q names a duration/latency measurement", name)
	case containsAny(lower,
		"resident_memory", "memory_bytes", "memory_usage", "cpu_seconds", "cpu_usage",
		"disk_", "filesystem_", "saturation", "utilization", "utilisation",
		"queue_length", "queue_size", "open_fds", "goroutines", "connections_in_use", "pool_"):
		return SignalSaturation, true, fmt.Sprintf("metric name %q names resource utilisation", name)
	case containsAny(lower,
		"requests_total", "request_count", "requests_count", "operations_total", "calls_total", "ops_total"):
		return SignalRate, true, fmt.Sprintf("metric name %q is a generic request/operation counter", name)
	}
	return "", false, ""
}

// codeLabelNames are the label names commonly used to carry an HTTP/gRPC
// status on a request counter. Order does not matter; the first one present
// on the expression is used.
var codeLabelNames = []string{"code", "status", "status_code", "grpc_code", "response_code"}

// classifyRequestCounterBySelector resolves the traffic-vs-errors ambiguity
// of a generic request counter by looking at what its own status/code-like
// selector is pinned to: a matcher whose values all look like 4xx/5xx names
// errors; all 2xx/3xx names traffic; a mix of both, or no such selector at
// all, is left ambiguous rather than guessed.
func classifyRequestCounterBySelector(pe ParsedExpr) (sig Signal, certain bool, reason string, ok bool) {
	for _, label := range codeLabelNames {
		values, present := pe.Matchers[label]
		if !present {
			continue
		}
		var hasFailure, hasSuccess bool
		for _, v := range values {
			f, s := codeClasses(v)
			hasFailure = hasFailure || f
			hasSuccess = hasSuccess || s
		}
		switch {
		case hasFailure && hasSuccess:
			return SignalErrors, false, fmt.Sprintf(
				"selector on %q matches both success- and failure-like values (%s); cannot tell whether "+
					"this rule tracks traffic or errors", label, strings.Join(values, ", ")), true
		case hasFailure:
			return SignalErrors, true, fmt.Sprintf(
				"selector on %q matches failure-like values (%s)", label, strings.Join(values, ", ")), true
		case hasSuccess:
			return SignalRate, true, fmt.Sprintf(
				"selector on %q matches success-like values (%s)", label, strings.Join(values, ", ")), true
		}
	}
	return "", false, "", false
}

// codeClasses reads a status-code-shaped matcher value (which may be a
// plain value or a "|"-separated regexp alternation, e.g. "5.."  or
// "4..|5..") and reports which of the two broad classes -- failure-like
// (4xx/5xx) or success-like (2xx/3xx) -- it touches. This is a heuristic
// over the value's leading digit, not a real status-code parser: it is
// deliberately simple, and wrong in the same direction a human skimming the
// rule would be wrong.
func codeClasses(v string) (hasFailure, hasSuccess bool) {
	for _, alt := range strings.Split(v, "|") {
		alt = strings.TrimSpace(strings.Trim(alt, "^$()"))
		if alt == "" {
			continue
		}
		switch alt[0] {
		case '4', '5':
			hasFailure = true
		case '2', '3':
			hasSuccess = true
		}
	}
	return hasFailure, hasSuccess
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// detectGenerator recognises Sloth- and Pyrra-generated rules by the label
// conventions each project's own rule generation documents, rather than by
// matching on alert or rule names (which a team can freely rename).
//
// Sloth: every rule it generates -- both the recording rules and the
// multiwindow alerting rules -- carries labels prefixed "sloth_"
// (sloth_id, sloth_service, sloth_slo, sloth_severity, sloth_window, ...).
// Presence of any "sloth_"-prefixed label on the alert is treated as
// conclusive.
//
// Pyrra: generated alerting rules carry a label named "slo" holding the
// objective's name, and its recording-rule chain uses metric names
// containing ":burnrate" (e.g. "http_requests:burnrate5m"). "slo" alone is
// too generic a label name to trust by itself -- a hand-written rule could
// use it coincidentally -- so both conditions are required together.
//
// This is best-effort against the conventions as documented at the time
// this was written; a customised template from either project, or a future
// convention change, may not match. Where it does not match, a burn-rate
// rule is simply classified by its metric name like any other rule (a
// recording rule named "...:burnrate5m" already contains "duration"? no --
// it falls through to "unclassifiable" and is reported as such, which is
// the correct failure mode: silence, not a wrong guess).
func detectGenerator(r Rule, pe ParsedExpr) (generator, evidence string) {
	for k := range r.Labels {
		if strings.HasPrefix(k, "sloth_") {
			return "sloth", fmt.Sprintf("its %q label", k)
		}
	}
	if _, ok := r.Labels["slo"]; ok {
		for _, metric := range pe.Metrics {
			if strings.Contains(metric, ":burnrate") {
				return "pyrra", fmt.Sprintf("its \"slo\" label combined with the recording-rule metric %q", metric)
			}
		}
	}
	return "", ""
}
