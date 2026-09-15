package webhook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

// fakeStore is a minimal in-memory Store for collector tests that do not
// need real reconciliation semantics (those are covered exhaustively at
// the store layer in internal/store/webhook_test.go). It tracks calls so
// tests can assert on collector-level behaviour: rule creation, fingerprint
// computation, state/status mapping.
type fakeStore struct {
	rulesByName map[string]int64
	nextRuleID  int64
	upserts     []upsertCall
}

type upsertCall struct {
	ep   store.Episode
	meta store.WebhookMeta
}

func newFakeStore() *fakeStore {
	return &fakeStore{rulesByName: map[string]int64{}}
}

func (f *fakeStore) RuleIDByAlertName(ctx context.Context, alertName string) (int64, bool, error) {
	id, ok := f.rulesByName[alertName]
	return id, ok, nil
}

func (f *fakeStore) UpsertRule(ctx context.Context, r *store.Rule) (int64, error) {
	f.nextRuleID++
	f.rulesByName[r.AlertName] = f.nextRuleID
	return f.nextRuleID, nil
}

func (f *fakeStore) UpsertWebhookEpisode(ctx context.Context, ep store.Episode, meta store.WebhookMeta) (int64, bool, error) {
	f.upserts = append(f.upserts, upsertCall{ep: ep, meta: meta})
	return int64(len(f.upserts)), false, nil
}

func loadPayload(t *testing.T, name string) Payload {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var p Payload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("fixture %s failed Validate: %v", name, err)
	}
	return p
}

func TestIngestGroupedMultiAlertCreatesOneEpisodePerAlert(t *testing.T) {
	fs := newFakeStore()
	c := New(fs)
	c.nowFunc = func() time.Time { return time.Date(2026, 9, 15, 9, 1, 0, 0, time.UTC) }

	p := loadPayload(t, "grouped_multialert.json")
	res, err := c.Ingest(context.Background(), p)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.AlertsProcessed != 2 {
		t.Errorf("AlertsProcessed = %d, want 2", res.AlertsProcessed)
	}
	if len(fs.upserts) != 2 {
		t.Fatalf("got %d upserts, want 2", len(fs.upserts))
	}
	if fs.upserts[0].ep.Fingerprint == fs.upserts[1].ep.Fingerprint {
		t.Errorf("two alerts differing only by instance must get different fingerprints")
	}
	for _, u := range fs.upserts {
		if u.ep.Source != store.SourceWebhook {
			t.Errorf("Source = %q, want %q", u.ep.Source, store.SourceWebhook)
		}
		if u.ep.State != store.StateFiring {
			t.Errorf("State = %q, want %q", u.ep.State, store.StateFiring)
		}
		if u.meta.Receiver != "team-pager" {
			t.Errorf("meta.Receiver = %q, want team-pager", u.meta.Receiver)
		}
		if u.meta.GroupKey != p.GroupKey {
			t.Errorf("meta.GroupKey = %q, want %q", u.meta.GroupKey, p.GroupKey)
		}
		// Still firing: EndsAt is Alertmanager's auto-resolve estimate, not
		// a measurement, so the episode's end must be "now", not that
		// estimate.
		if !u.ep.EndedAt.Equal(c.nowFunc()) {
			t.Errorf("EndedAt = %s, want now (%s) for a still-firing alert", u.ep.EndedAt, c.nowFunc())
		}
	}
	if _, ok := fs.rulesByName["DemoCauseA"]; !ok {
		t.Errorf("no rule created for DemoCauseA")
	}
}

func TestIngestResolvedUsesExactEndsAt(t *testing.T) {
	fs := newFakeStore()
	c := New(fs)
	c.nowFunc = func() time.Time { return time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC) }

	p := loadPayload(t, "resolved.json")
	if _, err := c.Ingest(context.Background(), p); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(fs.upserts) != 1 {
		t.Fatalf("got %d upserts, want 1", len(fs.upserts))
	}
	got := fs.upserts[0].ep
	wantEnd := p.Alerts[0].EndsAt
	if !got.EndedAt.Equal(wantEnd) {
		t.Errorf("EndedAt = %s, want the payload's exact endsAt %s", got.EndedAt, wantEnd)
	}
	if !got.StartedAt.Equal(p.Alerts[0].StartsAt) {
		t.Errorf("StartedAt = %s, want %s", got.StartedAt, p.Alerts[0].StartsAt)
	}
}

func TestIngestFingerprintExcludesAlertnameAndAlertstate(t *testing.T) {
	fs := newFakeStore()
	c := New(fs)
	c.nowFunc = time.Now

	p := Payload{
		Status: statusFiring, Receiver: "r", GroupKey: "g",
		Alerts: []Alert{{
			Status: statusFiring,
			Labels: map[string]string{
				"alertname":  "X",
				"alertstate": "firing",
				"instance":   "host-1",
			},
			StartsAt: time.Now(),
		}},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := c.Ingest(context.Background(), p); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	got := fs.upserts[0].ep.Labels
	if _, ok := got["alertname"]; ok {
		t.Errorf("Labels still carries alertname: %v", got)
	}
	if _, ok := got["alertstate"]; ok {
		t.Errorf("Labels still carries alertstate: %v", got)
	}
	if got["instance"] != "host-1" {
		t.Errorf("Labels lost instance: %v", got)
	}
}

func TestIngestReusesExistingRuleWithoutCreatingAnother(t *testing.T) {
	fs := newFakeStore()
	fs.rulesByName["DemoCauseA"] = 42
	c := New(fs)
	c.nowFunc = time.Now

	p := loadPayload(t, "grouped_multialert.json")
	if _, err := c.Ingest(context.Background(), p); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	for _, u := range fs.upserts {
		if u.ep.RuleID != 42 {
			t.Errorf("RuleID = %d, want the pre-existing rule id 42", u.ep.RuleID)
		}
	}
}
