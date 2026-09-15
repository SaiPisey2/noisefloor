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

	// What a rule's own status/code selector is pinned to is evidence about
	// every rule that carries one, not only about generic request counters,
	// so it is read once here and weighed against each metric name below.
	selSig, selCertain, selReason, hasSel := classifyBySelector(pe)

	for _, metric := range pe.Metrics {
		sig, certain, reason := classifyMetricName(metric)
		if sig == "" {
			continue
		}
		switch {
		case !hasSel:
			// Nothing to weigh the name against; the name stands.
		case sig == SignalRate:
			// The one shape genuinely ambiguous by name alone: a generic
			// request/operation counter could be tracking volume or
			// failures depending entirely on which values its own
			// selectors pin a status/code-shaped label to. The selector is
			// not merely evidence here, it is the answer.
			sig, certain, reason = selSig, selCertain, selReason
		case sig == selSig:
			// Name and selector agree. Say so -- a confident call is worth
			// explaining too, not just an uncertain one.
			reason = fmt.Sprintf("%s; %s", reason, selReason)
		default:
			// Name and selector disagree: `http_request_duration_seconds_count
			// {status=~"5.."}` is a duration metric by name and an error
			// rule by selector, and it is an ordinary way to write one.
			// Letting the name win silently claimed latency coverage the
			// service did not have and denied the error coverage it did.
			// The selector says what the rule SELECTS FOR, which is the
			// better guide to what it alerts on, so it decides the bucket
			// -- but a disagreement is exactly the evidence supporting more
			// than one reading that Certain exists to flag.
			reason = fmt.Sprintf(
				"%s, but %s -- name and selector disagree, so this is classified as %s from the "+
					"selector rather than as %s from the name", reason, selReason, selSig, sig)
			sig, certain = selSig, false
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
// (request/operation counters), and never allowed to overrule a
// failure-indicating selector on any shape -- see classifyBySelector.
func classifyMetricName(name string) (sig Signal, certain bool, reason string) {
	lower := strings.ToLower(name)
	switch {
	case lower == "up":
		return SignalRate, true, "\"up\" is the standard Prometheus target-availability metric"
	case strings.Contains(lower, "error"):
		return SignalErrors, true, fmt.Sprintf("metric name %q contains \"error\"", name)
	case containsAny(lower,
		"latency", "duration", "response_time", "request_time",
		// Two widespread conventions that time requests without using
		// either word: Micrometer/Spring Boot's http_server_requests_seconds
		// and go-grpc-prometheus's grpc_server_handling_seconds. Both are
		// ordinary latency histograms, and not recognising them meant a
		// Spring Boot or gRPC service read as having no latency alerting
		// however many it had.
		"requests_seconds", "handling_seconds"):
		return SignalLatency, true, fmt.Sprintf("metric name %q names a duration/latency measurement", name)
	case containsAny(lower,
		"resident_memory", "memory_bytes", "memory_usage", "cpu_seconds", "cpu_usage",
		"disk_", "filesystem_", "saturation", "utilization", "utilisation",
		"queue_length", "queue_size", "open_fds", "goroutines", "connections_in_use", "pool_"):
		return SignalSaturation, true, fmt.Sprintf("metric name %q names resource utilisation", name)
	case containsAny(lower,
		"requests_total", "request_count", "requests_count", "operations_total", "calls_total", "ops_total",
		// go-grpc-prometheus counts completed RPCs as
		// grpc_server_handled_total, with the status on a grpc_code label:
		// a request counter in every respect except the word "request".
		"handled_total"):
		return SignalRate, true, fmt.Sprintf("metric name %q is a generic request/operation counter", name)
	}
	return "", false, ""
}

// CodeLabelNames are the label names commonly used to carry an HTTP/gRPC
// status on a request counter. Order does not matter; the first one present
// on the expression is used.
//
// Exported so internal/pr's coverage-blind-spot starter templates can find
// the same status label on a service's own raw metric that this package
// would recognise on a hand-written rule's selector -- one list, not two
// that can quietly drift apart.
var CodeLabelNames = []string{"code", "status", "status_code", "grpc_code", "response_code"}

// classifyBySelector reads what a rule's own status/code-like selector is
// pinned to: a matcher that selects 4xx/5xx values indicates errors, one
// that selects 2xx/3xx indicates traffic, a matcher touching both classes,
// or no such selector at all, is left ambiguous rather than guessed.
//
// A negated matcher is read through its negation rather than dropped.
// `{code!~"2.."}` is one of the two ordinary ways to write an error rule --
// enumerate the failures, or exclude the successes -- and it says exactly
// as much about the rule's subject as `{code=~"5.."}` does. Treating it as
// absent left such a rule reading as a plain traffic counter, confidently.
// The inversion only runs one way per class: excluding success-like values
// selects failures, and excluding failure-like values selects the traffic
// that is left.
func classifyBySelector(pe ParsedExpr) (sig Signal, certain bool, reason string, ok bool) {
	for _, label := range CodeLabelNames {
		matchers, present := pe.Matchers[label]
		if !present {
			continue
		}
		var hasFailure, hasSuccess bool
		written := make([]string, 0, len(matchers))
		for _, m := range matchers {
			f, s := codeClasses(m.Value)
			if m.Negative {
				// Negating a value that touches both classes says nothing
				// usable about either.
				if f && s {
					continue
				}
				f, s = s, f
				written = append(written, "not "+m.Value)
			} else {
				written = append(written, m.Value)
			}
			hasFailure = hasFailure || f
			hasSuccess = hasSuccess || s
		}
		switch {
		case hasFailure && hasSuccess:
			return SignalErrors, false, fmt.Sprintf(
				"selector on %q selects both success- and failure-like values (%s); cannot tell whether "+
					"this rule tracks traffic or errors", label, strings.Join(written, ", ")), true
		case hasFailure:
			return SignalErrors, true, fmt.Sprintf(
				"selector on %q selects failure-like values (%s)", label, strings.Join(written, ", ")), true
		case hasSuccess:
			return SignalRate, true, fmt.Sprintf(
				"selector on %q selects success-like values (%s)", label, strings.Join(written, ", ")), true
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
