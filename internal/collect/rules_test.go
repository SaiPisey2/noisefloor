package collect

import (
	"context"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

type ruleStore struct {
	byKey  map[string]*store.Rule
	nextID int64
	kept   []int64
}

func newRuleStore() *ruleStore {
	return &ruleStore{byKey: map[string]*store.Rule{}, nextID: 1}
}

func (s *ruleStore) UpsertRule(_ context.Context, r *store.Rule) (int64, error) {
	key := r.GroupName + "/" + r.AlertName
	if existing, ok := s.byKey[key]; ok {
		id := existing.ID
		cp := *r
		cp.ID = id
		s.byKey[key] = &cp
		return id, nil
	}
	cp := *r
	cp.ID = s.nextID
	s.nextID++
	s.byKey[key] = &cp
	return cp.ID, nil
}

func (s *ruleStore) ListRules(context.Context) ([]store.Rule, error) {
	var out []store.Rule
	for _, r := range s.byKey {
		out = append(out, *r)
	}
	return out, nil
}

func (s *ruleStore) MarkRulesInactive(_ context.Context, keep []int64) error {
	s.kept = keep
	return nil
}

func TestSyncRulesUpsertsAlertingRules(t *testing.T) {
	ctx := context.Background()
	db := newRuleStore()
	now := time.Unix(1_700_000_000, 0).UTC()

	groups := []prom.RuleGroup{{
		Name: "demo", File: "/etc/prometheus/rules/demo.yml",
		Alerting: []prom.AlertingRule{{
			Name: "DemoSpiky", Query: "demo_spiky_gauge > 0",
			For:    30 * time.Second,
			Labels: map[string]string{"severity": "warning"},
		}},
	}}

	res, err := SyncRules(ctx, groups, db, now)
	if err != nil {
		t.Fatalf("SyncRules: %v", err)
	}
	if res.Active != 1 {
		t.Errorf("active = %d, want 1", res.Active)
	}

	got := db.byKey["demo/DemoSpiky"]
	if got == nil {
		t.Fatal("rule not stored under group/alertname")
	}
	if got.For != 30*time.Second {
		t.Errorf("for = %v, want 30s", got.For)
	}
	if got.File != "/etc/prometheus/rules/demo.yml" {
		t.Errorf("file = %q", got.File)
	}
	if got.ExprHash == "" {
		t.Error("expr hash not set")
	}
	if !got.Active {
		t.Error("rule should be active")
	}
}

func TestSyncRulesDeactivatesMissingRules(t *testing.T) {
	ctx := context.Background()
	db := newRuleStore()
	now := time.Unix(1_700_000_000, 0).UTC()

	// A rule that existed before but is no longer defined in Prometheus.
	stale := &store.Rule{AlertName: "Gone", GroupName: "demo", Active: true}
	staleID, _ := db.UpsertRule(ctx, stale)

	groups := []prom.RuleGroup{{
		Name: "demo",
		Alerting: []prom.AlertingRule{{Name: "Alive", Query: "up == 0"}},
	}}

	res, err := SyncRules(ctx, groups, db, now)
	if err != nil {
		t.Fatalf("SyncRules: %v", err)
	}
	if res.Deactivated != 1 {
		t.Errorf("deactivated = %d, want 1", res.Deactivated)
	}
	for _, id := range db.kept {
		if id == staleID {
			t.Error("stale rule was kept active")
		}
	}
}

func TestRetunedDuringDetectsMidWindowChange(t *testing.T) {
	windowStart := time.Unix(1_700_000_000, 0).UTC()

	unchanged := store.Rule{ExprChangedAt: windowStart.Add(-48 * time.Hour)}
	if RetunedDuring(unchanged, windowStart) {
		t.Error("a rule last changed before the window is not retuned during it")
	}

	retuned := store.Rule{ExprChangedAt: windowStart.Add(24 * time.Hour)}
	if !RetunedDuring(retuned, windowStart) {
		t.Error("a rule changed inside the window must be flagged as retuned")
	}

	never := store.Rule{}
	if RetunedDuring(never, windowStart) {
		t.Error("a rule with no recorded change must not be flagged")
	}
}

func TestExprHashChangesWithExpression(t *testing.T) {
	a := ExprHash("up == 0")
	b := ExprHash("up == 1")
	if a == b {
		t.Error("different expressions must hash differently")
	}
	if a != ExprHash("up == 0") {
		t.Error("hash must be stable for the same expression")
	}
	if len(a) != 16 {
		t.Errorf("hash length = %d, want 16", len(a))
	}
}
