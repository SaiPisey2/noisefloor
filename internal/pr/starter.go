// Starter proposals close the other half of remediation: internal/coverage
// finds a service with no alert on some signal at all, and this file turns
// that gap into a proposed rule, reusing every other piece of this
// package -- Proposal, Edit, BranchName, Provider, Run's opening logic --
// rather than building a second bot.
//
// Deleting or tuning a rule (Build, above) is a LOW-risk proposal: the
// evidence is thirty days of that rule's own behaviour, and the worst case
// is losing an alert nobody acted on. Proposing a brand NEW rule is the
// opposite. There is no firing history for a rule that has never existed,
// so a candidate threshold cannot be measured the way every other number
// this project emits is -- it can only ever be a conservative guess, or
// (where the service's own current metrics allow it) a number derived from
// today's behaviour, which is a very different claim from a measured
// history and has to say so.
//
// Three rules keep this honest:
//
//  1. Conservative by construction: every template picks a threshold an
//     order of magnitude looser than "this would obviously fire" -- a
//     starter rule that pages on day one teaches a team to ignore
//     noisefloor before it has proven anything.
//  2. Never silent about what is and is not evidence. A retire/tune body
//     states measured history throughout. A starter body has exactly one
//     measured fact (the coverage gap itself, and where possible today's
//     own value the threshold was scaled from) and says, explicitly, that
//     the threshold is a starting point requiring tuning -- never a
//     recommendation.
//  3. Never a guess about WHERE the rule goes. See resolveTarget's doc
//     comment for the placement policy and why it refuses rather than
//     invents a file or group.
package pr

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/remediate"
)

// KindPropose is a starter-rule proposal for a coverage blind spot, as
// opposed to KindRetire/KindTune which act on a rule that already exists.
const KindPropose Kind = "propose"

// Starter-proposal refusal reasons, reported through the same Refusal type
// and WriteResult rendering retire/tune refusals use -- see Run's doc
// comment on why that reuse matters. Group carries the service name and
// AlertName carries the signal for these, since there is no rule (group,
// alertname) yet to name.
const (
	ReasonIdle RefusalReason = "idle: this service's traffic proxy is negligible; a starter alert here " +
		"would fire on background noise, not on load worth paging for"
	ReasonNoMetric          RefusalReason = "this service exposes no metric the signal's starter template recognises"
	ReasonUncertainCoverage RefusalReason = "existing coverage on this signal is a guess, not certain; " +
		"propose will not add a second rule on top of a classification it cannot confirm"
	ReasonNoTarget RefusalReason = "could not determine which existing rule group to add this rule to"
)

// StarterPriority is the order propose tries signals in for one blind-spot
// service, taking the first one both uncovered and supported by the
// service's own metrics. A single run proposes at most ONE starter rule per
// service -- mirroring "never a PR touching more than one rule" from the
// retire/tune bot, scaled up one level: a service that has never had any
// alerting at all does not get four simultaneous pull requests the first
// time noisefloor looks at it.
//
// Rate first: "does this exist at all" is the most fundamental question a
// team can ask about a service, an absent() check requires no derived
// threshold at all (see rateTemplate), and it is what the demo's own
// checkout fixture leads with (CheckoutNoTraffic). Errors and latency
// follow in roughly the order an on-call engineer would investigate a
// silent service; saturation (resource pressure, not directly
// user-visible) is last.
var StarterPriority = []coverage.Signal{
	coverage.SignalRate, coverage.SignalErrors, coverage.SignalLatency, coverage.SignalSaturation,
}

// probeWindow is the range instant queries such as rate() or
// histogram_quantile() average over when deriving a starter threshold from
// a service's own current behaviour. Wider than a typical alert's own
// evaluation window on purpose: a starter proposal is a one-off snapshot
// taken once, not a continuously re-evaluated rule, and a wider window
// smooths over the kind of short traffic dip that would otherwise bias a
// brand-new threshold low before it has ever run for real.
const probeWindow = 30 * time.Minute

// starterMeasurement is what probing Prometheus found for one (service,
// signal) pair: whether the template's metric exists on this service at
// all, and -- when it does -- the value a threshold was scaled from.
type starterMeasurement struct {
	Supported  bool
	MetricUsed string // the metric name the template picked, for the PR body
	Selector   string // e.g. `job="search"` or `namespace="shop",service="checkout"`
	CodeLabel  string // errors only: which CodeLabelNames entry was found

	// Current is the presently observed value the candidate threshold was
	// derived from (an error ratio, a p99 latency in seconds, a resident
	// memory reading in bytes). Meaningless unless Derived is true.
	Current float64
	// Derived is true when Current came from a real Prometheus reading --
	// traffic existed in probeWindow to measure. False means there was
	// nothing to measure (e.g. zero requests just now) and the template
	// fell back to a fixed floor instead; the PR body must say which,
	// since "derived from your own p99" and "a hardcoded default" are very
	// different claims about the same-looking number.
	Derived bool
}

// serviceSelector returns the PromQL label matcher fragment that names one
// service the same way internal/coverage's own resolveTargets would
// recognise it: (namespace, service) for a Kubernetes-discovered service,
// job otherwise. Building the proposed rule's selector this way is what
// guarantees noisefloor coverage will see the new rule as covering this
// exact service (Scope "matched") once it is merged, rather than proposing
// a rule that accidentally covers nothing or everything.
func serviceSelector(s coverage.Service) string {
	if s.Source == "k8s" {
		return fmt.Sprintf("namespace=%q,service=%q", s.Namespace, s.Service)
	}
	return fmt.Sprintf("job=%q", s.Job)
}

// queryVector runs an instant query and returns it as a model.Vector,
// nil/empty (not an error) when Prometheus has nothing for it.
func queryVector(ctx context.Context, api prom.Client, query string, now time.Time) (model.Vector, error) {
	val, err := api.Query(ctx, query, now)
	if err != nil {
		return nil, fmt.Errorf("query %q: %w", query, err)
	}
	vec, ok := val.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("query %q: got %s, want vector", query, val.Type())
	}
	return vec, nil
}

// queryScalarSum runs an instant query and sums every returned series'
// value, reporting ok=false for an empty result (nothing to sum -- e.g. a
// ratio's denominator was zero) rather than a misleading 0.
func queryScalarSum(ctx context.Context, api prom.Client, query string, now time.Time) (value float64, ok bool, err error) {
	vec, err := queryVector(ctx, api, query, now)
	if err != nil {
		return 0, false, err
	}
	if len(vec) == 0 {
		return 0, false, nil
	}
	var sum float64
	any := false
	for _, s := range vec {
		if math.IsNaN(float64(s.Value)) {
			continue
		}
		sum += float64(s.Value)
		any = true
	}
	return sum, any, nil
}

// RequestCounterMetrics used by the rate/errors starter templates are
// exactly coverage.RequestCounters -- the same names internal/coverage's
// own classifier recognises as traffic, so a starter rule never names a
// metric coverage itself would not credit as "this service has traffic".
var requestCounterMetrics = coverage.RequestCounters

// durationHistogramMetrics are the request/operation duration histogram
// naming conventions the latency starter template looks for. Prometheus's
// histogram convention always exposes `<name>_bucket`; coverage.Classify
// recognises any metric name containing "duration" or "latency" by
// substring, and every name here contains "duration", so a rule built from
// one of these is also a rule coverage would itself classify as latency
// once it exists -- see verifyStarterClassification.
var durationHistogramMetrics = []string{
	"http_request_duration_seconds",
	"http_server_requests_seconds", // Micrometer / Spring Boot naming
	"grpc_server_handling_seconds",
}

// saturationMetrics are the resource-utilisation metric naming conventions
// the saturation starter template looks for. Deliberately narrow (one
// entry today): "resident memory above a threshold" is the one saturation
// shape common across arbitrary Go/JVM/Python services with no framework-
// specific convention to lean on; CPU, disk and queue-depth metrics vary
// too much by exporter to guess safely, so a service exposing only those
// is refused with ReasonNoMetric rather than guessed at. Extend this list,
// as coverage.classifyMetricName's own substring checks were, rather than
// adding a second mechanism.
var saturationMetrics = []string{"process_resident_memory_bytes"}

// probeRate finds the first request/operation counter this service exposes.
func probeRate(ctx context.Context, api prom.Client, svc coverage.Service, now time.Time) (starterMeasurement, error) {
	sel := serviceSelector(svc)
	for _, metric := range requestCounterMetrics {
		vec, err := queryVector(ctx, api, fmt.Sprintf("%s{%s}", metric, sel), now)
		if err != nil {
			return starterMeasurement{}, err
		}
		if len(vec) > 0 {
			return starterMeasurement{Supported: true, MetricUsed: metric, Selector: sel}, nil
		}
	}
	return starterMeasurement{}, nil
}

// probeErrors finds a request counter that also carries an HTTP-style
// status/code label (see coverage.CodeLabelNames), then measures the
// service's current error ratio over probeWindow so the candidate
// threshold can be scaled from it (see errorsTemplate).
//
// grpc_code is deliberately excluded from the label names tried here: gRPC
// status codes are small integers with no shared "this range means
// failure" convention the way HTTP's 5xx does, so a `=~"5.."` matcher
// would be silently wrong on it. A service whose only code-like label is
// grpc_code is refused with ReasonNoMetric rather than guessed at.
func probeErrors(ctx context.Context, api prom.Client, svc coverage.Service, now time.Time) (starterMeasurement, error) {
	sel := serviceSelector(svc)
	for _, metric := range requestCounterMetrics {
		vec, err := queryVector(ctx, api, fmt.Sprintf("%s{%s}", metric, sel), now)
		if err != nil {
			return starterMeasurement{}, err
		}
		if len(vec) == 0 {
			continue
		}
		codeLabel := findHTTPCodeLabel(vec)
		if codeLabel == "" {
			continue
		}
		errQuery := fmt.Sprintf(`sum(rate(%s{%s,%s=~"5.."}[%s])) / sum(rate(%s{%s}[%s]))`,
			metric, sel, codeLabel, formatRange(probeWindow), metric, sel, formatRange(probeWindow))
		current, derived, err := queryScalarSum(ctx, api, errQuery, now)
		if err != nil {
			return starterMeasurement{}, err
		}
		return starterMeasurement{
			Supported: true, MetricUsed: metric, Selector: sel, CodeLabel: codeLabel,
			Current: current, Derived: derived,
		}, nil
	}
	return starterMeasurement{}, nil
}

// findHTTPCodeLabel returns the first of coverage.CodeLabelNames (excluding
// grpc_code -- see probeErrors) present on any series in vec.
func findHTTPCodeLabel(vec model.Vector) string {
	for _, name := range coverage.CodeLabelNames {
		if name == "grpc_code" {
			continue
		}
		for _, s := range vec {
			if _, ok := s.Metric[model.LabelName(name)]; ok {
				return name
			}
		}
	}
	return ""
}

// probeLatency finds a request/operation duration histogram this service
// exposes, then measures its current p99 over probeWindow.
func probeLatency(ctx context.Context, api prom.Client, svc coverage.Service, now time.Time) (starterMeasurement, error) {
	sel := serviceSelector(svc)
	for _, metric := range durationHistogramMetrics {
		vec, err := queryVector(ctx, api, fmt.Sprintf("%s_bucket{%s}", metric, sel), now)
		if err != nil {
			return starterMeasurement{}, err
		}
		if len(vec) == 0 {
			continue
		}
		p99Query := fmt.Sprintf(`histogram_quantile(0.99, sum(rate(%s_bucket{%s}[%s])) by (le))`,
			metric, sel, formatRange(probeWindow))
		current, derived, err := queryScalarSum(ctx, api, p99Query, now)
		if err != nil {
			return starterMeasurement{}, err
		}
		return starterMeasurement{
			Supported: true, MetricUsed: metric, Selector: sel, Current: current, Derived: derived,
		}, nil
	}
	return starterMeasurement{}, nil
}

// probeSaturation finds a recognised resource-utilisation metric this
// service exposes, then reads its current value.
func probeSaturation(ctx context.Context, api prom.Client, svc coverage.Service, now time.Time) (starterMeasurement, error) {
	sel := serviceSelector(svc)
	for _, metric := range saturationMetrics {
		current, derived, err := queryScalarSum(ctx, api, fmt.Sprintf("sum(%s{%s})", metric, sel), now)
		if err != nil {
			return starterMeasurement{}, err
		}
		if !derived {
			continue // metric not exposed by this service at all
		}
		return starterMeasurement{
			Supported: true, MetricUsed: metric, Selector: sel, Current: current, Derived: true,
		}, nil
	}
	return starterMeasurement{}, nil
}

// probe dispatches to the right probeXxx for sig.
func probe(ctx context.Context, api prom.Client, svc coverage.Service, sig coverage.Signal, now time.Time) (starterMeasurement, error) {
	switch sig {
	case coverage.SignalRate:
		return probeRate(ctx, api, svc, now)
	case coverage.SignalErrors:
		return probeErrors(ctx, api, svc, now)
	case coverage.SignalLatency:
		return probeLatency(ctx, api, svc, now)
	case coverage.SignalSaturation:
		return probeSaturation(ctx, api, svc, now)
	default:
		return starterMeasurement{}, fmt.Errorf("no starter template for signal %q", sig)
	}
}

// formatRange renders a duration the way PromQL range-vector selectors
// spell it (5m, 30m, 1h) -- distinct from formatDuration, which is prose.
func formatRange(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int64(d/time.Minute))
}

// starterRule is one signal template's fully-resolved output: enough to
// render both the YAML block and the PR body.
type starterRule struct {
	AlertName string
	Expr      string // may be multi-line (joined with "\n") for a ratio expression
	For       time.Duration
	Severity  string
	Summary   string

	// ThresholdDerived is true when the numeric threshold in Expr was
	// scaled from starterMeasurement.Current (see each templateXxx
	// function); false when it is a fixed template default. Explained
	// prints the sentence the PR body states verbatim -- computed here,
	// next to the arithmetic it describes, rather than reconstructed from
	// raw numbers in the body renderer.
	ThresholdDerived bool
	Explained        string
}

// serviceIdent turns a coverage.Service.Name ("search", "shop/checkout")
// into a PascalCase identifier fragment ("Search", "ShopCheckout") suitable
// as an alert-name prefix.
func serviceIdent(name string) string {
	var b strings.Builder
	nextUpper := true
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			if nextUpper {
				b.WriteString(strings.ToUpper(string(r)))
				nextUpper = false
			} else {
				b.WriteRune(r)
			}
		default:
			nextUpper = true
		}
	}
	if b.Len() == 0 {
		return "Service"
	}
	return b.String()
}

// rateTemplate proposes a "traffic disappeared" check: absent() over the
// request counter this service exposes.
//
// Deliberately has NO threshold to derive at all -- absent() checks for the
// total lack of a series, not a value crossing a level, so there is
// nothing here that a service's own history could scale. That makes it the
// single safest starter template: it can be wrong about how LONG an outage
// has to last before paging (the `for:`), but it cannot be wrong about
// what "no traffic" means the way a guessed rate or latency threshold can.
func rateTemplate(svc coverage.Service, m starterMeasurement) starterRule {
	ident := serviceIdent(svc.Name)
	return starterRule{
		AlertName: ident + "NoTraffic",
		Expr:      fmt.Sprintf("absent(%s{%s})", m.MetricUsed, m.Selector),
		For:       10 * time.Minute,
		Severity:  "page",
		Summary:   fmt.Sprintf("%s has stopped reporting requests entirely", svc.Name),
		Explained: fmt.Sprintf(
			"absent() has no threshold to derive: it fires when the %s series for %s "+
				"disappears entirely, which is either true or it isn't.", m.MetricUsed, svc.Name),
	}
}

// errorRatioFloor is the fixed lower bound errorsTemplate applies to a
// scaled threshold, and the value used outright when there is no current
// traffic to scale from. 5% mirrors the demo's own hand-tuned
// CheckoutErrorRateHigh threshold -- a reasonable "obviously too lax
// rather than too tight" default for a service with no history at all.
const errorRatioFloor = 0.05

// errorRatioMultiple is how far above the CURRENTLY observed error ratio
// errorsTemplate sets its candidate threshold, when there is a current
// ratio to scale from. 3x a already-nonzero baseline still catches a real
// spike while giving several times today's noise floor of headroom before
// day one produces a page.
const errorRatioMultiple = 3

func errorsTemplate(svc coverage.Service, m starterMeasurement) starterRule {
	ident := serviceIdent(svc.Name)
	threshold := errorRatioFloor
	explained := fmt.Sprintf(
		"no requests observed in the last %s to measure a current error ratio from; "+
			"the threshold below is a fixed default (%s), not derived from this service at all.",
		formatRange(probeWindow), formatPercent(errorRatioFloor))
	derived := false
	if m.Derived {
		scaled := m.Current * errorRatioMultiple
		if scaled > threshold {
			threshold = scaled
			derived = true
			explained = fmt.Sprintf(
				"this service's own current error ratio, measured over the last %s, is %s; "+
					"the threshold below is %dx that (%s).",
				formatRange(probeWindow), formatPercent(m.Current), errorRatioMultiple, formatPercent(threshold))
		} else {
			explained = fmt.Sprintf(
				"this service's own current error ratio, measured over the last %s, is %s -- "+
					"%dx that would be below noisefloor's %s floor for a brand-new rule, so the "+
					"floor is used instead.",
				formatRange(probeWindow), formatPercent(m.Current), errorRatioMultiple, formatPercent(errorRatioFloor))
		}
	}
	expr := fmt.Sprintf(
		"sum(rate(%s{%s,%s=~\"5..\"}[5m]))\n/\nsum(rate(%s{%s}[5m])) > %s",
		m.MetricUsed, m.Selector, m.CodeLabel, m.MetricUsed, m.Selector, formatThreshold(threshold))
	return starterRule{
		AlertName:        ident + "ErrorRateHigh",
		Expr:             expr,
		For:              10 * time.Minute,
		Severity:         "page",
		Summary:          fmt.Sprintf("%s error rate above %s", svc.Name, formatPercent(threshold)),
		ThresholdDerived: derived,
		Explained:        explained,
	}
}

// latencyMultiple is how far above the CURRENTLY observed p99 latency
// latencyTemplate sets its candidate threshold, when there is traffic to
// scale from -- generous enough that ordinary variance in an already-
// healthy service does not page on day one.
const latencyMultiple = 2

// latencyFloor is used outright when there is no current traffic to
// measure a p99 from. 2s is a conservative "obviously slow" bar for an
// HTTP-shaped request with no history to say otherwise.
const latencyFloor = 2 * time.Second

func latencyTemplate(svc coverage.Service, m starterMeasurement) starterRule {
	ident := serviceIdent(svc.Name)
	threshold := latencyFloor
	explained := fmt.Sprintf(
		"no requests observed in the last %s to measure a current p99 latency from; "+
			"the threshold below is a fixed default (%s), not derived from this service at all.",
		formatRange(probeWindow), formatDuration(latencyFloor))
	derived := false
	if m.Derived {
		scaled := time.Duration(m.Current * latencyMultiple * float64(time.Second))
		if scaled > threshold {
			threshold = scaled
			derived = true
			explained = fmt.Sprintf(
				"this service's own current p99 latency, measured over the last %s, is %s; "+
					"the threshold below is %dx that (%s).",
				formatRange(probeWindow), formatDuration(time.Duration(m.Current*float64(time.Second))),
				latencyMultiple, formatDuration(threshold))
		} else {
			explained = fmt.Sprintf(
				"this service's own current p99 latency, measured over the last %s, is %s -- "+
					"%dx that would be below noisefloor's %s floor for a brand-new rule, so the "+
					"floor is used instead.",
				formatRange(probeWindow), formatDuration(time.Duration(m.Current*float64(time.Second))),
				latencyMultiple, formatDuration(latencyFloor))
		}
	}
	expr := fmt.Sprintf("histogram_quantile(0.99, sum(rate(%s_bucket{%s}[5m])) by (le)) > %s",
		m.MetricUsed, m.Selector, formatThreshold(threshold.Seconds()))
	return starterRule{
		AlertName:        ident + "LatencyHigh",
		Expr:             expr,
		For:              10 * time.Minute,
		Severity:         "warning",
		Summary:          fmt.Sprintf("%s p99 latency above %s", svc.Name, formatDuration(threshold)),
		ThresholdDerived: derived,
		Explained:        explained,
	}
}

// saturationMultiple is how far above the CURRENTLY observed reading
// saturationTemplate sets its candidate threshold.
const saturationMultiple = 1.5

// saturationFloor is used outright when the metric could not be read at
// all (probeSaturation only reports Supported when it DID read a value, so
// this is here purely as a defensive fallback -- see saturationTemplate).
const saturationFloor = 5e8 // 500MB, the demo's own hand-tuned convention

func saturationTemplate(svc coverage.Service, m starterMeasurement) starterRule {
	ident := serviceIdent(svc.Name)
	threshold := saturationFloor
	explained := fmt.Sprintf(
		"could not read a current value for %s; the threshold below is a fixed default "+
			"(%s), not derived from this service at all.", m.MetricUsed, formatBytes(saturationFloor))
	derived := false
	if m.Derived {
		scaled := m.Current * saturationMultiple
		if scaled > threshold {
			threshold = scaled
		}
		derived = true
		explained = fmt.Sprintf(
			"this service's own current %s reading is %s; the threshold below is %gx that (%s).",
			m.MetricUsed, formatBytes(m.Current), saturationMultiple, formatBytes(threshold))
	}
	expr := fmt.Sprintf("sum(%s{%s}) > %s", m.MetricUsed, m.Selector, formatThreshold(threshold))
	return starterRule{
		AlertName:        ident + "MemoryHigh",
		Expr:             expr,
		For:              15 * time.Minute,
		Severity:         "warning",
		Summary:          fmt.Sprintf("%s resident memory above %s", svc.Name, formatBytes(threshold)),
		ThresholdDerived: derived,
		Explained:        explained,
	}
}

func buildTemplate(sig coverage.Signal, svc coverage.Service, m starterMeasurement) starterRule {
	switch sig {
	case coverage.SignalRate:
		return rateTemplate(svc, m)
	case coverage.SignalErrors:
		return errorsTemplate(svc, m)
	case coverage.SignalLatency:
		return latencyTemplate(svc, m)
	case coverage.SignalSaturation:
		return saturationTemplate(svc, m)
	default:
		panic(fmt.Sprintf("buildTemplate: no template for signal %q", sig))
	}
}

// verifyStarterClassification runs a proposed rule's own expression back
// through internal/coverage's real classifier (Extract + Classify) and
// confirms it lands on the signal it was built for, with Certain true.
//
// This is the "never propose a rule referencing a metric that does not
// exist [or classifies unexpectedly]" safety net stated as code rather
// than as a comment: every template above is hand-written to match
// coverage's own recognised metric shapes, and this assertion is what
// catches the two from drifting apart silently if either one changes later
// without the other. It is not expected to ever fail in production; it
// exists to fail LOUDLY (as an error, refusing the proposal) rather than
// silently if it ever does.
func verifyStarterClassification(sig coverage.Signal, rule starterRule) error {
	pe, err := coverage.Extract(rule.Expr)
	if err != nil {
		return fmt.Errorf("starter template for %s produced an unparsable expression %q: %w", sig, rule.Expr, err)
	}
	match, ok := coverage.Classify(coverage.Rule{
		GroupName: "starter", AlertName: rule.AlertName, Expr: rule.Expr,
	}, pe)
	if !ok {
		return fmt.Errorf("starter template for %s produced an expression coverage cannot classify at all: %q", sig, rule.Expr)
	}
	if match.Signal != sig {
		return fmt.Errorf("starter template for %s produced an expression coverage classifies as %s instead: %q",
			sig, match.Signal, rule.Expr)
	}
	if !match.Certain {
		return fmt.Errorf("starter template for %s produced an expression coverage can only guess about (%s): %q",
			sig, match.Reason, rule.Expr)
	}
	return nil
}

// formatThreshold renders a raw float threshold compactly for a PromQL
// literal: no trailing zeros, but never scientific notation, which
// `expr:` lines in every hand-written rule in this project avoid.
func formatThreshold(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	return s
}

// formatBytes renders a byte count the way the demo's own rules do (a
// human-legible MB figure), while formatThreshold above renders the exact
// same number for the PromQL literal -- kept separate because the PR body
// prose reads better in MB than in raw bytes.
func formatBytes(v float64) string {
	return fmt.Sprintf("%.0fMB", v/1e6)
}

// StarterInput is everything BuildStarter needs for one (service, signal)
// candidate. Grid/Idle/CoverageBySignal are pre-computed by the caller
// (RunStarters) from a coverage.Result, kept separate from BuildStarter so
// its refusal logic is unit-testable without a Prometheus connection --
// only Measurement needs a real probe (or a fake prom.Client in tests).
type StarterInput struct {
	Service coverage.Service
	Signal  coverage.Signal

	// Idle mirrors coverage.BlindSpot.Idle for this service.
	Idle bool
	// ExistingMatches is sc.Covered[Signal] for this service -- empty for a
	// genuine blind spot. See ReasonUncertainCoverage: non-empty but
	// entirely non-Certain matches are refused rather than proposed over.
	ExistingMatches []coverage.RuleMatch

	Measurement starterMeasurement

	// Target is where to add the rule -- see resolveTarget. Zero value
	// (Target{}) means no target could be determined.
	Target Target
}

// Target names an existing rule group to append a starter rule to: a file
// already on disk, and a group name already defined in it.
type Target struct {
	File  string
	Group string
}

// BuildStarter turns one StarterInput into either a Proposal or a Refusal,
// mirroring Build's contract for retire/tune: both nil means nothing
// applies (not reachable here in practice, since a caller only calls this
// for a signal it already knows is uncovered -- kept symmetric with Build
// regardless).
//
// Order: existing-but-uncertain coverage, then idle, then unsupported
// metric, then no resolvable target. Each is independent of the others, so
// this order is "cheapest and least surprising first" rather than a
// dependency chain -- unlike Build's retire/tune ordering, which mirrors a
// real precondition chain (a location has to exist before drift against it
// can be checked).
func BuildStarter(in StarterInput) (*Proposal, *Refusal, error) {
	name, sig := in.Service.Name, in.Signal

	if len(in.ExistingMatches) > 0 {
		anyCertain := false
		for _, m := range in.ExistingMatches {
			if m.Certain {
				anyCertain = true
			}
		}
		if anyCertain {
			return nil, nil, nil // genuinely covered; nothing to propose
		}
		return nil, &Refusal{Group: name, AlertName: string(sig), Reason: ReasonUncertainCoverage}, nil
	}

	if in.Idle {
		return nil, &Refusal{Group: name, AlertName: string(sig), Reason: ReasonIdle}, nil
	}
	if !in.Measurement.Supported {
		return nil, &Refusal{Group: name, AlertName: string(sig), Reason: ReasonNoMetric}, nil
	}
	if in.Target.File == "" || in.Target.Group == "" {
		return nil, &Refusal{Group: name, AlertName: string(sig), Reason: ReasonNoTarget}, nil
	}

	rule := buildTemplate(sig, in.Service, in.Measurement)
	if err := verifyStarterClassification(sig, rule); err != nil {
		return nil, nil, err
	}

	edit, err := starterEdit(in.Target, rule)
	if err != nil {
		return nil, nil, err
	}

	body := StarterBody(in.Service, sig, rule, in.Measurement, in.Target)
	branch := BranchName(KindPropose, name, rule.AlertName, "v1")

	return &Proposal{
		Kind: KindPropose, Group: in.Target.Group, AlertName: rule.AlertName,
		File:   in.Target.File,
		Branch: branch,
		Title:  fmt.Sprintf("noisefloor: propose starter %s rule for %s", sig, name),
		Body:   body,
		Edit:   edit,
	}, nil, nil
}

// starterEdit locates the last rule in target's group (by EndLine) and
// inserts the new rule block immediately after it, indented to match that
// rule's own indentation exactly -- the same "copy a real sibling's
// indentation" approach tuneEdit uses when inserting a for: line with no
// field to replace.
func starterEdit(target Target, rule starterRule) (Edit, error) {
	locs, fileErrs, err := remediate.LocateRules(targetRoot(target.File))
	if err != nil {
		return Edit{}, err
	}
	_ = fileErrs // a target file that fails to parse surfaces via resolveTarget before this is ever called

	var last *remediate.RuleLocation
	for i := range locs {
		l := &locs[i]
		if l.File != target.File || l.Key.Group != target.Group {
			continue
		}
		if last == nil || l.EndLine > last.EndLine {
			last = l
		}
	}
	if last == nil {
		return Edit{}, fmt.Errorf("starterEdit: group %q not found in %s (should have been caught by resolveTarget)",
			target.Group, target.File)
	}

	anchorLine, err := readLine(target.File, last.StartLine)
	if err != nil {
		return Edit{}, err
	}
	dashIndent := indentOf(anchorLine)
	fieldIndentStr := dashIndent + "  "

	lines := []string{
		"", // blank line separating this rule from its predecessor, matching house style
		dashIndent + "- alert: " + rule.AlertName,
	}
	exprLines := strings.Split(rule.Expr, "\n")
	if len(exprLines) == 1 {
		lines = append(lines, fieldIndentStr+"expr: "+exprLines[0])
	} else {
		lines = append(lines, fieldIndentStr+"expr: |")
		for _, l := range exprLines {
			lines = append(lines, fieldIndentStr+"  "+l)
		}
	}
	lines = append(lines,
		fieldIndentStr+"for: "+formatDuration(rule.For),
		fieldIndentStr+fmt.Sprintf("labels: {severity: %s}", rule.Severity),
		fieldIndentStr+fmt.Sprintf("annotations: {summary: %q}", rule.Summary),
	)

	return NewInsertBlock(target.File, last.EndLine, lines), nil
}

func indentOf(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " "))]
}

// targetRoot returns the directory LocateRules should walk to find file --
// its own immediate parent, so starterEdit does not need cfg.Rules.Path
// threaded all the way down to it and re-parses only the one directory the
// target actually lives in.
func targetRoot(file string) string {
	if i := strings.LastIndexByte(file, '/'); i >= 0 {
		return file[:i]
	}
	return "."
}

// StarterBody renders a starter proposal's PR body. Unlike RetireBody and
// TuneBody, most of what this states is NOT a measurement -- see this
// file's package doc comment -- so the one thing this function must never
// do is let that distinction blur. Every number that came from Prometheus
// (the traffic proxy, and -- when Measurement.Derived -- today's own error
// ratio/p99/resource reading) is labelled as measured; the threshold
// itself is always labelled a starting point.
func StarterBody(svc coverage.Service, sig coverage.Signal, rule starterRule, m starterMeasurement, target Target) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## noisefloor: propose starter %s rule for `%s`\n\n", sig, svc.Name)
	fmt.Fprintf(&b,
		"`%s` carries no alert coverage on **%s** at all -- confirmed by `noisefloor coverage`, "+
			"which found no rule anywhere in this checkout whose expression names this service on "+
			"this signal. This proposes a first, minimal starter rule to close that gap.\n\n",
		svc.Name, sig)

	b.WriteString("> [!WARNING]\n" +
		"> **The gap above is measured. The threshold below is not.** Every other proposal " +
		"noisefloor opens (a retire or a tune) states a number derived from thirty days of a " +
		"rule's own firing history. This rule has never existed, so there is no history to derive " +
		"a threshold from -- it is a conservative starting point, not a recommendation. " +
		"Expect to tune it after it has run for a while.\n\n")

	basis := m.Selector
	fmt.Fprintf(&b, "| what | value | measured? |\n| --- | --- | --- |\n")
	fmt.Fprintf(&b, "| traffic proxy | %.2f %s | yes -- this is why the service was ranked as a blind spot worth acting on |\n",
		svc.Traffic, svc.TrafficBasis)
	fmt.Fprintf(&b, "| selector used | `%s` | -- |\n", basis)
	fmt.Fprintf(&b, "| proposed `for:` | %s | no -- a fixed template default |\n", formatDuration(rule.For))
	if m.Derived {
		fmt.Fprintf(&b, "| proposed threshold | see expression below | **partially** -- scaled from this service's own current reading, see below |\n")
	} else {
		fmt.Fprintf(&b, "| proposed threshold | see expression below | no -- a fixed template default, see below |\n")
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "**Where the threshold comes from:** %s\n\n", rule.Explained)

	fmt.Fprintf(&b, "**Proposed rule** (appended to group `%s` in `%s`):\n\n```yaml\n- alert: %s\n",
		target.Group, target.File, rule.AlertName)
	exprLines := strings.Split(rule.Expr, "\n")
	if len(exprLines) == 1 {
		fmt.Fprintf(&b, "  expr: %s\n", exprLines[0])
	} else {
		b.WriteString("  expr: |\n")
		for _, l := range exprLines {
			fmt.Fprintf(&b, "    %s\n", l)
		}
	}
	fmt.Fprintf(&b, "  for: %s\n  labels: {severity: %s}\n  annotations: {summary: %q}\n```\n\n",
		formatDuration(rule.For), rule.Severity, rule.Summary)

	fmt.Fprintf(&b,
		"**How to check this**\n\nRun the metric this rule reads against your own Prometheus and "+
			"confirm it exists and looks the way this body claims:\n\n    %s{%s}\n\nnoisefloor verified "+
			"this metric exists for `%s` before proposing this rule; it does not verify that the "+
			"THRESHOLD is right for your traffic pattern, because nothing could -- that is what "+
			"tuning this rule after it has run for a while is for.\n\n",
		m.MetricUsed, m.Selector, svc.Name)

	b.WriteString("---\nGenerated by noisefloor from a coverage scan, not from firing history: " +
		"there is none yet for a rule that has never existed. Review the threshold before merging, " +
		"and revisit it with `noisefloor scan` once this rule has run for a while.\n")
	return b.String()
}
