package coverage

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// ParsedExpr is everything Extract pulls out of one PromQL expression by
// walking its AST -- never by matching the expression's text. Regex over the
// raw string produces confident wrong answers (a metric name inside a string
// literal, a label matcher split across a line continuation, a commented-out
// clause) which is worse than not extracting anything: this feature's entire
// output is "you have no alert for this", and a false negative or false
// positive there is exactly the failure that gets a tool uninstalled.
type ParsedExpr struct {
	// Metrics is every metric name referenced by a vector or matrix
	// selector in the expression, deduplicated and sorted. A recording
	// rule's own name is not one of these -- Extract only sees the
	// expression, not what it is assigned to.
	Metrics []string

	// Matchers is the union, across every selector in the expression, of
	// label name -> the distinct matchers applied to it, each carrying how
	// it matched (equality/regexp, positive/negative) rather than only its
	// value. Two selectors naming different metrics but sharing a label (a
	// common pattern in a ratio expression, e.g. errors{code=~"5.."} /
	// total{}) both contribute to the same map, which is what lets a bare
	// "total" selector and a "5xx" selector in the same expression jointly
	// signal "this rule is about errors" even though neither selector alone
	// would.
	//
	// Negative matchers (!= and !~) are collected here too, and the reason
	// is not symmetry for its own sake: `{code!~"2.."}` is how a large
	// share of real error rules are written, and dropping it left the
	// disambiguator blind to the one label that says what such a rule is
	// about -- it read as a plain traffic counter, confidently. A caller
	// that only wants the labels a rule NAMES (rather than every label it
	// mentions) asks for Positive; a caller reasoning about what the rule
	// selects for, as classification does, reads Negative too.
	Matchers map[string][]Matcher

	// HasSubquery is true when the expression contains a subquery
	// (expr[range:step]) rather than only plain range/instant selectors.
	HasSubquery bool

	// AbsentFuncs is every absent()/absent_over_time() call found, by
	// function name. A rule built around absent() is checking for the lack
	// of a series -- typically "traffic disappeared entirely" -- which is a
	// meaningfully different shape from a threshold comparison over an
	// existing series, even though it reads the same metric.
	AbsentFuncs []string

	// Grouping is every label named in a `by (...)` or `without (...)`
	// clause of an aggregation, deduplicated. Used to detect a rule that
	// aggregates across all services of a job/namespace (e.g. `by (job)`)
	// rather than naming one explicitly in a matcher -- see MapRules.
	Grouping []string
	// GroupingWithout mirrors whether any AggregateExpr in the expression
	// used `without` rather than `by`: `without (le)` still groups by every
	// OTHER label including job/namespace/service, so it must not be read
	// the same way as `by (le)`, which drops everything except le.
	GroupingWithout bool
	// HasAggregation is whether the expression aggregates at all. An
	// expression that does not (e.g. `up == 0`) drops no label whatsoever,
	// so every job/namespace/service label its selectors carry survives
	// into the alert -- a distinct case from `sum(...)` with an empty `by`,
	// which collapses everything into one unattributable number. Grouping
	// alone cannot tell the two apart: both leave it empty.
	HasAggregation bool
}

// Matcher is one label matcher preserved from a selector: the value it was
// written with, plus how it was applied.
type Matcher struct {
	Value string
	// Negative is true for != and !~.
	Negative bool
	// Regexp is true for =~ and !~. Prometheus anchors regexp matchers at
	// both ends, and Matches does the same.
	Regexp bool
}

// Matches reports whether m selects the label value target, evaluating a
// regexp matcher as Prometheus itself would (fully anchored) rather than
// approximating it. Negation is applied last, so `code!~"2.."` matches
// exactly the values `code=~"2.."` does not.
func (m Matcher) Matches(target string) bool {
	var hit bool
	if m.Regexp {
		re, err := regexp.Compile("^(?:" + m.Value + ")$")
		if err != nil {
			// Unreachable via Extract: the PromQL parser compiles every
			// regexp matcher before this package ever sees it. Treating an
			// uncompilable pattern as matching nothing keeps a caller from
			// acting on a match that was never evaluated.
			return false
		}
		hit = re.MatchString(target)
	} else {
		hit = m.Value == target
	}
	return hit != m.Negative
}

// Positive returns the values of every non-negated matcher on label, and
// whether there were any. This is what a caller asking "which service does
// this rule NAME" wants: `job!="prometheus"` mentions a job without naming
// the one the rule is about.
func (pe ParsedExpr) Positive(label string) ([]string, bool) {
	var out []string
	for _, m := range pe.Matchers[label] {
		if !m.Negative {
			out = append(out, m.Value)
		}
	}
	return out, len(out) > 0
}

// Extract parses expr with prometheus/promql/parser and walks the resulting
// AST. Returns an error if expr does not parse -- callers must not fall back
// to any text-based guess when that happens, and must report the rule as
// unclassifiable rather than silently skipping it.
// promParser has no state of its own (Options{} is the parser's defaults,
// same as every alert rule Prometheus itself evaluates) and NewParser
// returns a value safe for concurrent use, so one shared instance is fine.
var promParser = parser.NewParser(parser.Options{})

func Extract(expr string) (ParsedExpr, error) {
	node, err := promParser.ParseExpr(expr)
	if err != nil {
		return ParsedExpr{}, fmt.Errorf("parse promql: %w", err)
	}

	pe := ParsedExpr{Matchers: map[string][]Matcher{}}
	metricSeen := map[string]bool{}
	matcherSeen := map[string]map[Matcher]bool{}
	groupSeen := map[string]bool{}
	absentSeen := map[string]bool{}

	parser.Inspect(node, func(n parser.Node, _ []parser.Node) error {
		switch v := n.(type) {
		case *parser.VectorSelector:
			if v.Name != "" && !metricSeen[v.Name] {
				metricSeen[v.Name] = true
				pe.Metrics = append(pe.Metrics, v.Name)
			}
			for _, m := range v.LabelMatchers {
				if m.Name == labels.MetricName {
					// __name__=... selectors are metric name spelled as a
					// matcher rather than as VectorSelector.Name; they are
					// already captured above via v.Name for the common case,
					// and matching against a mix of names is nobody's
					// question here.
					continue
				}
				pm := Matcher{
					Value:    m.Value,
					Negative: m.Type == labels.MatchNotEqual || m.Type == labels.MatchNotRegexp,
					Regexp:   m.Type == labels.MatchRegexp || m.Type == labels.MatchNotRegexp,
				}
				if matcherSeen[m.Name] == nil {
					matcherSeen[m.Name] = map[Matcher]bool{}
				}
				if !matcherSeen[m.Name][pm] {
					matcherSeen[m.Name][pm] = true
					pe.Matchers[m.Name] = append(pe.Matchers[m.Name], pm)
				}
			}
		case *parser.SubqueryExpr:
			pe.HasSubquery = true
		case *parser.AggregateExpr:
			pe.HasAggregation = true
			for _, g := range v.Grouping {
				if !groupSeen[g] {
					groupSeen[g] = true
					pe.Grouping = append(pe.Grouping, g)
				}
			}
			if v.Without {
				pe.GroupingWithout = true
			}
		case *parser.Call:
			if v.Func != nil && (v.Func.Name == "absent" || v.Func.Name == "absent_over_time") {
				if !absentSeen[v.Func.Name] {
					absentSeen[v.Func.Name] = true
					pe.AbsentFuncs = append(pe.AbsentFuncs, v.Func.Name)
				}
			}
		}
		return nil
	})

	sort.Strings(pe.Metrics)
	sort.Strings(pe.Grouping)
	sort.Strings(pe.AbsentFuncs)
	for k := range pe.Matchers {
		ms := pe.Matchers[k]
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].Value != ms[j].Value {
				return ms[i].Value < ms[j].Value
			}
			if ms[i].Negative != ms[j].Negative {
				return !ms[i].Negative
			}
			return !ms[i].Regexp && ms[j].Regexp
		})
	}
	return pe, nil
}
