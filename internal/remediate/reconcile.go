package remediate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
)

// FindingKind classifies a discrepancy between what a git checkout's rule
// files define and what Prometheus is actually evaluating.
type FindingKind string

const (
	// MissingInFiles is a rule Prometheus reports via /api/v1/rules that no
	// file under rules.path defines. This is expected to happen -- a stale
	// checkout, the wrong rules.path, a rule loaded from somewhere this
	// checkout does not cover -- and is reported as a finding, never a
	// crash: the whole point of reconciling against the live API is to
	// catch exactly this before a remediation PR is opened against the
	// wrong (or no) file.
	MissingInFiles FindingKind = "missing_in_files"
	// MissingInPrometheus is a rule found in a file that Prometheus does
	// not evaluate -- a rule file not on Prometheus's rule_files glob, a
	// syntax error keeping the group from loading, or a checkout that is
	// ahead of what is actually deployed.
	MissingInPrometheus FindingKind = "missing_in_prometheus"
	// Drifted is a rule that exists on both sides under the same (group,
	// alertname) but whose file no longer says what Prometheus is
	// evaluating. Matching key sets is not the same as matching rules: a
	// checkout can be behind by exactly one commit that changed a `for:`,
	// and every key still lines up.
	//
	// This is the difference between a PR body that reads "Raise `for: 1m`
	// to `for: 6m`" -- taken from the live rule that was actually scored --
	// sitting above a diff that reads `- for: 45s`, because the file says
	// something else. The evidence and the edit are then about two
	// different rules, and no reviewer can be expected to spot that.
	Drifted FindingKind = "drifted"
)

// Finding is one reconciliation discrepancy between file locations and the
// live Prometheus rule set.
type Finding struct {
	Kind      FindingKind
	Group     string
	AlertName string
	// File is set for MissingInPrometheus and Drifted: the file the rule was
	// found in. MissingInFiles has, by definition, no file to name.
	File string
	// Detail names both sides of a Drifted finding -- what the file says and
	// what Prometheus evaluates -- so the discrepancy is readable without a
	// second lookup. Empty for every other kind.
	Detail string
}

func (f Finding) String() string {
	switch f.Kind {
	case MissingInFiles:
		return fmt.Sprintf(
			"%s/%s is evaluated by prometheus but not found under the configured rules path "+
				"(stale checkout, or rules.path pointed at the wrong place?)",
			f.Group, f.AlertName)
	case MissingInPrometheus:
		return fmt.Sprintf(
			"%s/%s is defined in %s but prometheus does not evaluate it "+
				"(not on prometheus's rule_files glob, failing to load, or the checkout is ahead of what's deployed)",
			f.Group, f.AlertName, f.File)
	case Drifted:
		return fmt.Sprintf(
			"%s/%s in %s does not match the rule prometheus evaluates (%s); the checkout is "+
				"stale or ahead, so its scored history describes a different rule",
			f.Group, f.AlertName, f.File, f.Detail)
	default:
		return fmt.Sprintf("%s: %s/%s", f.Kind, f.Group, f.AlertName)
	}
}

// DriftAgainst compares the rule as the checkout writes it against the rule
// as it is actually evaluated, and returns a description of every field that
// disagrees (naming both values) plus whether there was any disagreement at
// all.
//
// `expr` is compared semantically, not textually. `store.Rule.Expr` comes
// back from Prometheus as `rule.Query().String()` -- a canonical re-render
// of the parsed AST -- while a repository writes PromQL however its authors
// like: `up==0` with no spaces, `{job="x", instance="y"}` where Prometheus
// prints matchers comma-joined with none, `0.50` where Prometheus prints
// `0.5`, or a `#` comment inside an `expr: |` block. None of those survive
// whitespace-collapsing text comparison, which made the check refuse nearly
// every hand-written rule. So both sides are parsed with promql/parser and
// compared by the canonical String() of the parsed expression -- the same
// renderer Prometheus itself used for liveExpr -- which absorbs all of the
// above. Critically, it also stops collapsing whitespace *inside* a string
// literal: {job="a b"} and {job="a  b"} are different selectors, and
// re-rendering from the parsed matcher value (not raw source text) keeps
// them different.
//
// If either side fails to parse as PromQL, that is reported as drift in its
// own right, naming which side failed -- falling back to a text comparison
// would silently readopt the very defect this replaces.
//
// `for` is compared as a duration rather than as text, so `60s` and `1m` --
// the same rule, written two ways -- do not read as drift. An unparseable
// `for:` in the file IS reported: noisefloor cannot claim the file agrees
// with Prometheus when it cannot tell what the file says.
func DriftAgainst(loc RuleLocation, liveExpr string, liveFor time.Duration) (string, bool) {
	var parts []string

	fileCanon, fileErr := canonicalExpr(loc.ExprValue)
	liveCanon, liveErr := canonicalExpr(liveExpr)
	switch {
	case fileErr != nil && liveErr != nil:
		parts = append(parts, fmt.Sprintf("%s/%s: neither side's expr parses as PromQL "+
			"(file: %v; prometheus: %v)", loc.Key.Group, loc.Key.AlertName, fileErr, liveErr))
	case fileErr != nil:
		parts = append(parts, fmt.Sprintf("%s/%s: file expr does not parse as PromQL (%v); "+
			"prometheus evaluates %q", loc.Key.Group, loc.Key.AlertName, fileErr, collapseWS(liveExpr)))
	case liveErr != nil:
		parts = append(parts, fmt.Sprintf("%s/%s: prometheus's expr does not parse as PromQL (%v); "+
			"file says %q", loc.Key.Group, loc.Key.AlertName, liveErr, collapseWS(loc.ExprValue)))
	case fileCanon != liveCanon:
		parts = append(parts, fmt.Sprintf("file expr %q, prometheus evaluates %q",
			collapseWS(loc.ExprValue), collapseWS(liveExpr)))
	}

	fileFor, err := parseForValue(loc.ForValue)
	switch {
	case err != nil:
		parts = append(parts, fmt.Sprintf("file `for: %s` is not a valid duration (%v), "+
			"prometheus evaluates `for: %s`", loc.ForValue, err, formatDuration(liveFor)))
	case fileFor != liveFor:
		parts = append(parts, fmt.Sprintf("file `for: %s`, prometheus evaluates `for: %s`",
			formatDuration(fileFor), formatDuration(liveFor)))
	}

	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "; "), true
}

// parseForValue reads a rule file's `for:` scalar. An absent `for:` is zero,
// which is exactly what Prometheus reports for a rule without one, so the
// two compare equal without a special case.
func parseForValue(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := model.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return time.Duration(d), nil
}

// promqlParser is stateless across calls (see promql/parser.NewParser); one
// instance is reused rather than constructed per comparison.
var promqlParser = parser.NewParser(parser.Options{})

// canonicalExpr parses expr as PromQL and renders it back through the
// parser's own String(), so two spellings of the same query compare equal
// by construction -- see DriftAgainst for why this replaced text
// comparison.
func canonicalExpr(expr string) (string, error) {
	e, err := promqlParser.ParseExpr(expr)
	if err != nil {
		return "", err
	}
	return e.String(), nil
}

// collapseWS reduces every run of whitespace to a single space and trims the
// ends. Used only for display in drift messages now -- the equality check
// itself is semantic (see canonicalExpr) -- so an expression that fails to
// parse can still be quoted readably in the finding.
func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// Reconcile compares rule locations found on disk against the rule groups
// Prometheus reports live, and returns every (group, alertname) that
// appears on only one side -- plus, for the ones that appear on both, every
// rule whose file content no longer matches what is evaluated (Drifted).
//
// Comparing key sets alone is not reconciliation. It answers "is this rule
// in both places", which a checkout one commit behind passes cleanly while
// every `for:` and `expr:` in it is out of date. The Drifted arm is the half
// that actually checks the two sides say the same thing.
//
// Order is deterministic: MissingInFiles first, then MissingInPrometheus,
// then Drifted, each sorted by group then alert name, so two runs against
// unchanged inputs produce identical output -- this ends up in CI logs,
// where a shuffled order would look like flapping data rather than a stable
// report.
func Reconcile(locs []RuleLocation, groups []prom.RuleGroup) []Finding {
	fileKeys := make(map[RuleKey]RuleLocation, len(locs))
	for _, l := range locs {
		fileKeys[l.Key] = l
	}

	promRules := make(map[RuleKey]prom.AlertingRule)
	for _, g := range groups {
		for _, r := range g.Alerting {
			promRules[RuleKey{Group: g.Name, AlertName: r.Name}] = r
		}
	}

	var missingInFiles, missingInProm, drifted []Finding
	for k := range promRules {
		if _, ok := fileKeys[k]; !ok {
			missingInFiles = append(missingInFiles, Finding{Kind: MissingInFiles, Group: k.Group, AlertName: k.AlertName})
		}
	}
	for k, loc := range fileKeys {
		live, ok := promRules[k]
		if !ok {
			missingInProm = append(missingInProm, Finding{Kind: MissingInPrometheus, Group: k.Group, AlertName: k.AlertName, File: loc.File})
			continue
		}
		if detail, drift := DriftAgainst(loc, live.Query, live.For); drift {
			drifted = append(drifted, Finding{
				Kind: Drifted, Group: k.Group, AlertName: k.AlertName, File: loc.File, Detail: detail,
			})
		}
	}

	byGroupThenName := func(fs []Finding) {
		sort.Slice(fs, func(i, j int) bool {
			if fs[i].Group != fs[j].Group {
				return fs[i].Group < fs[j].Group
			}
			return fs[i].AlertName < fs[j].AlertName
		})
	}
	byGroupThenName(missingInFiles)
	byGroupThenName(missingInProm)
	byGroupThenName(drifted)

	findings := make([]Finding, 0, len(missingInFiles)+len(missingInProm)+len(drifted))
	findings = append(findings, missingInFiles...)
	findings = append(findings, missingInProm...)
	findings = append(findings, drifted...)
	return findings
}
