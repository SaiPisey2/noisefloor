package remediate

import (
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
)

func TestReconcile(t *testing.T) {
	// DemoSpiky carries a matching expr on both sides: this test is about
	// key-set matching (missing in files / missing in prometheus), not
	// drift, and an empty expr on both sides would itself fail to parse as
	// PromQL and register as a spurious Drifted finding.
	locs := []RuleLocation{
		{Key: RuleKey{Group: "demo", AlertName: "DemoSpiky"}, File: "demo.yml", ExprValue: "up == 0"},
		{Key: RuleKey{Group: "demo", AlertName: "OrphanFileRule"}, File: "demo.yml", ExprValue: "up == 0"},
	}
	groups := []prom.RuleGroup{
		{
			Name: "demo",
			Alerting: []prom.AlertingRule{
				{Name: "DemoSpiky", Query: "up == 0"},
				{Name: "OrphanPromRule"},
			},
		},
	}

	findings := Reconcile(locs, groups)
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}

	// MissingInFiles sorts first.
	if findings[0].Kind != MissingInFiles || findings[0].AlertName != "OrphanPromRule" {
		t.Errorf("findings[0] = %+v, want MissingInFiles/OrphanPromRule", findings[0])
	}
	if findings[1].Kind != MissingInPrometheus || findings[1].AlertName != "OrphanFileRule" {
		t.Errorf("findings[1] = %+v, want MissingInPrometheus/OrphanFileRule", findings[1])
	}
	if findings[1].File != "demo.yml" {
		t.Errorf("findings[1].File = %q, want demo.yml", findings[1].File)
	}

	for _, f := range findings {
		if f.String() == "" {
			t.Error("Finding.String() must not be empty")
		}
	}
}

func TestReconcile_noDiscrepancies(t *testing.T) {
	locs := []RuleLocation{
		{Key: RuleKey{Group: "demo", AlertName: "DemoSpiky"}, File: "demo.yml", ExprValue: "up == 0"},
	}
	groups := []prom.RuleGroup{
		{Name: "demo", Alerting: []prom.AlertingRule{{Name: "DemoSpiky", Query: "up == 0"}}},
	}
	if findings := Reconcile(locs, groups); len(findings) != 0 {
		t.Errorf("got %d findings for matching input, want 0: %+v", len(findings), findings)
	}
}

func TestReconcile_deterministicOrder(t *testing.T) {
	groups := []prom.RuleGroup{
		{Name: "b", Alerting: []prom.AlertingRule{{Name: "Z"}, {Name: "A"}}},
		{Name: "a", Alerting: []prom.AlertingRule{{Name: "Q"}}},
	}
	findings := Reconcile(nil, groups)
	if len(findings) != 3 {
		t.Fatalf("got %d findings, want 3", len(findings))
	}
	want := []RuleKey{{Group: "a", AlertName: "Q"}, {Group: "b", AlertName: "A"}, {Group: "b", AlertName: "Z"}}
	for i, f := range findings {
		got := RuleKey{Group: f.Group, AlertName: f.AlertName}
		if got != want[i] {
			t.Errorf("findings[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

// TestReconcileDetectsDrift is the half of reconciliation that matching key
// sets cannot do: both sides define demo/Stale, and they disagree about
// what it is.
func TestReconcileDetectsDrift(t *testing.T) {
	locs := []RuleLocation{
		{Key: RuleKey{Group: "demo", AlertName: "Stale"}, File: "demo.yml",
			ExprValue: "up == 0", ForValue: "45s"},
		{Key: RuleKey{Group: "demo", AlertName: "Fresh"}, File: "demo.yml",
			ExprValue: "up == 1", ForValue: "2m"},
	}
	groups := []prom.RuleGroup{{Name: "demo", Alerting: []prom.AlertingRule{
		{Name: "Stale", Query: "up == 0", For: time.Minute},
		{Name: "Fresh", Query: "up == 1", For: 2 * time.Minute},
	}}}

	findings := Reconcile(locs, groups)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Kind != Drifted || f.AlertName != "Stale" {
		t.Fatalf("finding = %+v, want Drifted/Stale", f)
	}
	// Both values, so the discrepancy is readable without opening the file.
	if !strings.Contains(f.Detail, "45s") || !strings.Contains(f.Detail, "1m") {
		t.Errorf("detail %q does not name both for: values", f.Detail)
	}
	if !strings.Contains(f.String(), "demo.yml") {
		t.Errorf("finding does not name the file: %s", f.String())
	}
}

// TestDriftAgainstToleratesEquivalentSpellings keeps the check off
// differences that are not differences: Prometheus normalises durations and
// re-serialises expressions from their parsed form.
func TestDriftAgainstToleratesEquivalentSpellings(t *testing.T) {
	loc := RuleLocation{
		ExprValue: "sum(rate(errors[5m]))\n  /\n  sum(rate(requests[5m])) > 0.1\n",
		ForValue:  "120s",
	}
	detail, drift := DriftAgainst(loc, "sum(rate(errors[5m])) / sum(rate(requests[5m])) > 0.1", 2*time.Minute)
	if drift {
		t.Errorf("reported drift on equivalent spellings: %s", detail)
	}
}

// TestDriftAgainstReportsAnUnparseableFor: noisefloor cannot claim the file
// agrees with Prometheus when it cannot tell what the file says.
func TestDriftAgainstReportsAnUnparseableFor(t *testing.T) {
	loc := RuleLocation{ExprValue: "up == 0", ForValue: "5 minutes"}
	detail, drift := DriftAgainst(loc, "up == 0", 5*time.Minute)
	if !drift {
		t.Fatal("an unparseable for: was treated as agreement")
	}
	if !strings.Contains(detail, "5 minutes") {
		t.Errorf("detail %q does not quote the unparseable value", detail)
	}
}

// TestDriftAgainstTreatsAbsentForAsZero: a rule with no `for:` and a
// Prometheus rule reporting 0 are the same rule, not drift.
func TestDriftAgainstTreatsAbsentForAsZero(t *testing.T) {
	loc := RuleLocation{ExprValue: "up == 0", ForValue: ""}
	if detail, drift := DriftAgainst(loc, "up == 0", 0); drift {
		t.Errorf("reported drift for a rule with no for: on either side: %s", detail)
	}
}

// TestCanonicalExprPairsThatMustCompareEqual is the "too strict" half of the
// drift-check defect: store.Rule.Expr is Prometheus's canonical re-render of
// the parsed AST, which differs from ordinary hand-written PromQL in
// exactly these ways. A checker that cannot see past them refuses on nearly
// every hand-written rule file, which is what collapseWS text comparison
// did.
func TestCanonicalExprPairsThatMustCompareEqual(t *testing.T) {
	cases := []struct {
		name, file, live, reason string
	}{
		{
			name: "no spaces around the operator",
			file: "up==0", live: "up == 0",
			reason: "Prometheus's canonical renderer always spaces binary operators; " +
				"an author who doesn't is still writing the same rule",
		},
		{
			name: "matcher comma spacing",
			file: `{job="x", instance="y"}`, live: `{job="x",instance="y"}`,
			reason: "Prometheus prints matchers comma-joined with no space after the comma; " +
				"a space there does not change which series are selected",
		},
		{
			name: "number formatting",
			file: "up == 0.50", live: "up == 0.5",
			reason: "0.50 and 0.5 are the same float64 -- only comparing the parsed value " +
				"(not the source text) sees that",
		},
		{
			name: "trailing comment",
			file: "up == 0 # why this threshold", live: "up == 0",
			reason: "a PromQL comment is not part of the expression the parser produces, " +
				"so String() drops it on both sides",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fc, err := canonicalExpr(c.file)
			if err != nil {
				t.Fatalf("file side failed to parse: %v", err)
			}
			lc, err := canonicalExpr(c.live)
			if err != nil {
				t.Fatalf("live side failed to parse: %v", err)
			}
			if fc != lc {
				t.Errorf("canonical forms %q vs %q differ but should be equal: %s", fc, lc, c.reason)
			}
		})
	}
}

// TestCanonicalExprPairsThatMustCompareDifferent is the "too loose" half:
// collapsing whitespace collapsed it *inside string literals too*, which
// makes genuinely different selectors compare equal. Semantic comparison
// must not repeat that, and must still catch ordinary rule changes.
func TestCanonicalExprPairsThatMustCompareDifferent(t *testing.T) {
	cases := []struct {
		name, file, live, reason string
	}{
		{
			name: "whitespace inside a string literal is real",
			file: `{job="a b"}`, live: `{job="a  b"}`,
			reason: "these select different label values -- collapsing whitespace inside a " +
				"string literal was the exact over-normalisation defect being fixed",
		},
		{
			name: "different range window",
			file: "rate(x[5m])", live: "rate(x[10m])",
			reason: "a 5m vs 10m rate window is a materially different rule, not a spelling difference",
		},
		{
			name: "different comparison operator",
			file: "up > 0.1", live: "up >= 0.1",
			reason: "> and >= fire on different sets of samples and must never collapse-equal",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fc, err := canonicalExpr(c.file)
			if err != nil {
				t.Fatalf("file side failed to parse: %v", err)
			}
			lc, err := canonicalExpr(c.live)
			if err != nil {
				t.Fatalf("live side failed to parse: %v", err)
			}
			if fc == lc {
				t.Errorf("canonical forms matched (%q) but should differ: %s", fc, c.reason)
			}
		})
	}
}

// TestDriftAgainstRefusesOnAnUnparseableExpr: falling back to a text
// comparison when a side fails to parse would silently readopt the defect
// being fixed here, so a parse failure is itself reported as drift, naming
// the rule and which side failed.
func TestDriftAgainstRefusesOnAnUnparseableExpr(t *testing.T) {
	loc := RuleLocation{
		Key:       RuleKey{Group: "demo", AlertName: "Broken"},
		ExprValue: "up ===",
	}
	detail, drift := DriftAgainst(loc, "up == 0", 0)
	if !drift {
		t.Fatal("an unparseable file expr was treated as agreement")
	}
	if !strings.Contains(detail, "demo/Broken") {
		t.Errorf("detail %q does not name the rule", detail)
	}
	if !strings.Contains(detail, "file expr does not parse") {
		t.Errorf("detail %q does not say which side failed to parse", detail)
	}
}
