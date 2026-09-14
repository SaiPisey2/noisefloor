package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *SQLite {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUpsertRuleIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	r := &Rule{
		AlertName: "HighErrors",
		GroupName: "api",
		Expr:      `rate(errors[5m]) > 0.1`,
		For:       5 * time.Minute,
		Labels:    map[string]string{"severity": "page"},
		FirstSeen: now,
		LastSeen:  now,
		Active:    true,
	}
	id1, err := db.UpsertRule(ctx, r)
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	r.LastSeen = now.Add(time.Hour)
	id2, err := db.UpsertRule(ctx, r)
	if err != nil {
		t.Fatalf("second UpsertRule: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("ids differ: %d vs %d, upsert should reuse the row", id1, id2)
	}

	rules, err := db.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	if rules[0].Labels["severity"] != "page" {
		t.Errorf("labels not round-tripped: %v", rules[0].Labels)
	}
	if !rules[0].LastSeen.Equal(now.Add(time.Hour)) {
		t.Errorf("last_seen = %v, want updated value", rules[0].LastSeen)
	}
}

func TestInsertEpisodesDedupes(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	start := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: start, LastSeen: start, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	eps := []Episode{{
		RuleID:      id,
		Fingerprint: "fp1",
		Labels:      map[string]string{"instance": "a"},
		StartedAt:   start,
		EndedAt:     start.Add(5 * time.Minute),
		Resolution:  time.Minute,
		Source:      SourceBackfill,
		State:       StateFiring,
	}}
	if err := db.InsertEpisodes(ctx, eps); err != nil {
		t.Fatalf("InsertEpisodes: %v", err)
	}
	if err := db.InsertEpisodes(ctx, eps); err != nil {
		t.Fatalf("re-InsertEpisodes: %v", err)
	}

	got, err := db.ListEpisodes(ctx, id, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d episodes, want 1 after duplicate insert", len(got))
	}
	if got[0].Labels["instance"] != "a" {
		t.Errorf("labels not round-tripped: %v", got[0].Labels)
	}
	if !got[0].EndedAt.Equal(start.Add(5 * time.Minute)) {
		t.Errorf("ended_at = %v, want %v", got[0].EndedAt, start.Add(5*time.Minute))
	}
}

func TestListEpisodesRespectsWindow(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	id, _ := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})

	mk := func(offset time.Duration, fp string) Episode {
		return Episode{
			RuleID: id, Fingerprint: fp, StartedAt: base.Add(offset),
			EndedAt: base.Add(offset + time.Minute), Resolution: time.Minute,
			Source: SourceBackfill, State: StateFiring,
		}
	}
	if err := db.InsertEpisodes(ctx, []Episode{mk(0, "a"), mk(48*time.Hour, "b")}); err != nil {
		t.Fatalf("InsertEpisodes: %v", err)
	}

	got, err := db.ListEpisodes(ctx, id, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(got) != 1 || got[0].Fingerprint != "a" {
		t.Fatalf("window filter wrong: got %d episodes %+v", len(got), got)
	}
}

func TestUpsertSilencesRoundTripsMatchers(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	start := time.Unix(1_700_000_000, 0).UTC()

	s := Silence{
		AMID:      "sil-1",
		Matchers:  []Matcher{{Name: "alertname", Value: "A", IsEqual: true}},
		CreatedBy: "sai",
		Comment:   "too noisy",
		StartsAt:  start,
		EndsAt:    start.Add(24 * time.Hour),
	}
	if err := db.UpsertSilences(ctx, []Silence{s, s}); err != nil {
		t.Fatalf("UpsertSilences: %v", err)
	}
	got, err := db.ListSilences(ctx, start.Add(-time.Hour), start.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("ListSilences: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d silences, want 1", len(got))
	}
	if len(got[0].Matchers) != 1 || got[0].Matchers[0].Name != "alertname" {
		t.Errorf("matchers not round-tripped: %+v", got[0].Matchers)
	}
	if got[0].CreatedBy != "sai" {
		t.Errorf("created_by = %q, want sai", got[0].CreatedBy)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if _, err := db.Meta(ctx, "missing"); err == nil {
		t.Fatal("Meta on missing key succeeded, want error")
	}
	if err := db.SetMeta(ctx, "retention_floor", "1700000000"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := db.SetMeta(ctx, "retention_floor", "1700000001"); err != nil {
		t.Fatalf("SetMeta overwrite: %v", err)
	}
	v, err := db.Meta(ctx, "retention_floor")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if v != "1700000001" {
		t.Errorf("meta = %q, want 1700000001", v)
	}
}

func TestUpsertScoreReplaces(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	id, _ := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})

	sc := &Score{
		RuleID: id, WindowStart: base, WindowEnd: base.Add(24 * time.Hour),
		Signals: map[string]float64{"flap_rate": 0.4}, NoiseScore: 61.5,
		Verdict: "tune", Confidence: 0.8, ComputedAt: base,
	}
	if err := db.UpsertScore(ctx, sc); err != nil {
		t.Fatalf("UpsertScore: %v", err)
	}
	sc.NoiseScore = 70
	if err := db.UpsertScore(ctx, sc); err != nil {
		t.Fatalf("UpsertScore replace: %v", err)
	}
	got, err := db.ListScores(ctx, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("ListScores: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d scores, want 1", len(got))
	}
	if got[0].NoiseScore != 70 {
		t.Errorf("noise_score = %v, want 70", got[0].NoiseScore)
	}
	if got[0].Signals["flap_rate"] != 0.4 {
		t.Errorf("signals not round-tripped: %v", got[0].Signals)
	}
}

func TestRuleIDByAlertName(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()

	want, err := db.UpsertRule(ctx, &Rule{
		AlertName: "HighErrors", GroupName: "api",
		FirstSeen: base, LastSeen: base, Active: true,
	})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	got, found, err := db.RuleIDByAlertName(ctx, "HighErrors")
	if err != nil {
		t.Fatalf("RuleIDByAlertName: %v", err)
	}
	if !found {
		t.Fatal("existing rule not found by alert name")
	}
	if got != want {
		t.Errorf("id = %d, want %d", got, want)
	}

	if _, found, err = db.RuleIDByAlertName(ctx, "NoSuchRule"); err != nil {
		t.Fatalf("RuleIDByAlertName on missing rule returned error: %v", err)
	} else if found {
		t.Error("missing rule reported as found")
	}
}

func TestRuleIDByAlertNamePrefersActiveRule(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()

	// An orphan row from a deleted rule, and the live rule of the same name.
	_, _ = db.UpsertRule(ctx, &Rule{
		AlertName: "Dup", GroupName: "", FirstSeen: base, LastSeen: base, Active: false,
	})
	live, _ := db.UpsertRule(ctx, &Rule{
		AlertName: "Dup", GroupName: "api", FirstSeen: base, LastSeen: base, Active: true,
	})

	got, found, err := db.RuleIDByAlertName(ctx, "Dup")
	if err != nil || !found {
		t.Fatalf("lookup failed: found=%v err=%v", found, err)
	}
	if got != live {
		t.Errorf("id = %d, want the active rule %d", got, live)
	}
}

func TestMarkRulesInactive(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	keep, _ := db.UpsertRule(ctx, &Rule{AlertName: "Keep", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})
	_, _ = db.UpsertRule(ctx, &Rule{AlertName: "Gone", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})

	if err := db.MarkRulesInactive(ctx, []int64{keep}); err != nil {
		t.Fatalf("MarkRulesInactive: %v", err)
	}
	rules, _ := db.ListRules(ctx)
	for _, r := range rules {
		want := r.AlertName == "Keep"
		if r.Active != want {
			t.Errorf("rule %s active = %v, want %v", r.AlertName, r.Active, want)
		}
	}
}
