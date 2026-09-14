package remediate

import (
	"testing"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
)

func TestReconcile(t *testing.T) {
	locs := []RuleLocation{
		{Key: RuleKey{Group: "demo", AlertName: "DemoSpiky"}, File: "demo.yml"},
		{Key: RuleKey{Group: "demo", AlertName: "OrphanFileRule"}, File: "demo.yml"},
	}
	groups := []prom.RuleGroup{
		{
			Name: "demo",
			Alerting: []prom.AlertingRule{
				{Name: "DemoSpiky"},
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
		{Key: RuleKey{Group: "demo", AlertName: "DemoSpiky"}, File: "demo.yml"},
	}
	groups := []prom.RuleGroup{
		{Name: "demo", Alerting: []prom.AlertingRule{{Name: "DemoSpiky"}}},
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
