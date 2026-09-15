package coverage

import (
	"fmt"
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
	// label name -> the distinct values matched against it (from = and =~
	// matchers; != and !~ are exclusions and are not collected here, since
	// they narrow rather than name what the rule is about). Two selectors
	// naming different metrics but sharing a label (a common pattern in a
	// ratio expression, e.g. errors{code=~"5.."} / total{}) both contribute
	// to the same map, which is what lets a bare "total" selector and a
	// "5xx" selector in the same expression jointly signal "this rule is
	// about errors" even though neither selector alone would.
	Matchers map[string][]string

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

	pe := ParsedExpr{Matchers: map[string][]string{}}
	metricSeen := map[string]bool{}
	valueSeen := map[string]map[string]bool{}
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
				if m.Type != labels.MatchEqual && m.Type != labels.MatchRegexp {
					continue // != and !~ narrow, they do not name
				}
				if valueSeen[m.Name] == nil {
					valueSeen[m.Name] = map[string]bool{}
				}
				if !valueSeen[m.Name][m.Value] {
					valueSeen[m.Name][m.Value] = true
					pe.Matchers[m.Name] = append(pe.Matchers[m.Name], m.Value)
				}
			}
		case *parser.SubqueryExpr:
			pe.HasSubquery = true
		case *parser.AggregateExpr:
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
		sort.Strings(pe.Matchers[k])
	}
	return pe, nil
}
