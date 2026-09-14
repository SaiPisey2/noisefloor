package remediate

import (
	"fmt"
	"sort"

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
)

// Finding is one reconciliation discrepancy between file locations and the
// live Prometheus rule set.
type Finding struct {
	Kind      FindingKind
	Group     string
	AlertName string
	// File is set only for MissingInPrometheus: the file the rule was
	// found in. MissingInFiles has, by definition, no file to name.
	File string
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
	default:
		return fmt.Sprintf("%s: %s/%s", f.Kind, f.Group, f.AlertName)
	}
}

// Reconcile compares rule locations found on disk against the rule groups
// Prometheus reports live, and returns every (group, alertname) that
// appears on only one side. Order is deterministic: MissingInFiles first,
// then MissingInPrometheus, each sorted by group then alert name, so two
// runs against unchanged inputs produce identical output -- this ends up in
// PR bodies and CI logs, where a shuffled order would look like flapping
// data rather than a stable report.
func Reconcile(locs []RuleLocation, groups []prom.RuleGroup) []Finding {
	fileKeys := make(map[RuleKey]RuleLocation, len(locs))
	for _, l := range locs {
		fileKeys[l.Key] = l
	}

	promKeys := make(map[RuleKey]bool)
	for _, g := range groups {
		for _, r := range g.Alerting {
			promKeys[RuleKey{Group: g.Name, AlertName: r.Name}] = true
		}
	}

	var missingInFiles, missingInProm []Finding
	for k := range promKeys {
		if _, ok := fileKeys[k]; !ok {
			missingInFiles = append(missingInFiles, Finding{Kind: MissingInFiles, Group: k.Group, AlertName: k.AlertName})
		}
	}
	for k, loc := range fileKeys {
		if !promKeys[k] {
			missingInProm = append(missingInProm, Finding{Kind: MissingInPrometheus, Group: k.Group, AlertName: k.AlertName, File: loc.File})
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

	findings := make([]Finding, 0, len(missingInFiles)+len(missingInProm))
	findings = append(findings, missingInFiles...)
	findings = append(findings, missingInProm...)
	return findings
}
