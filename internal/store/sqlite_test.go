package store

import (
	"context"
	"path/filepath"
	"strings"
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

	got, err := db.ListEpisodesInWindow(ctx, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
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

	got, err := db.ListEpisodesInWindow(ctx, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
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

// TestUpsertScoreReplaces reads the row back through the raw *sql.DB rather
// than a store method: no exported reader of the scores table has a consumer
// outside this test (ListScores was removed as dead API surface), and adding
// one back just to assert here would recreate the thing being avoided.
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

	rows, err := db.db.QueryContext(ctx,
		`SELECT rule_id, noise_score, signals FROM scores WHERE rule_id = ?`, id)
	if err != nil {
		t.Fatalf("query scores: %v", err)
	}
	defer rows.Close()

	var count int
	var noiseScore float64
	var signals string
	for rows.Next() {
		count++
		if err := rows.Scan(&id, &noiseScore, &signals); err != nil {
			t.Fatalf("scan score: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("got %d score rows, want 1", count)
	}
	if noiseScore != 70 {
		t.Errorf("noise_score = %v, want 70", noiseScore)
	}
	if !strings.Contains(signals, `"flap_rate":0.4`) {
		t.Errorf("signals not round-tripped: %v", signals)
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

func TestUpsertRuleObservesChangedExpressionOnly(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()

	// First upsert: rule with expression A
	// In production, ExprHash would come from the collect package,
	// but we use a simple stable hash here for testing.
	hashA := "hashA"
	hashB := "hashB"

	r1 := &Rule{
		AlertName: "Test", GroupName: "g",
		Expr:      "up == 0",
		ExprHash:  hashA,
		FirstSeen: base, LastSeen: base,
		Active: true,
	}
	_, _ = db.UpsertRule(ctx, r1)

	rules, _ := db.ListRules(ctx)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	if !rules[0].ExprChangedAt.IsZero() {
		t.Error("new rule should have zero expr_changed_at on insert")
	}

	// Second upsert: same rule, same expression, different LastSeen
	r2 := &Rule{
		AlertName: "Test", GroupName: "g",
		Expr:      "up == 0",
		ExprHash:  hashA,
		FirstSeen: base, LastSeen: base.Add(time.Hour),
		Active: true,
	}
	_, _ = db.UpsertRule(ctx, r2)

	rules, _ = db.ListRules(ctx)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	if !rules[0].ExprChangedAt.IsZero() {
		t.Error("expr_changed_at should stay zero when expression doesn't change")
	}

	// Third upsert: same rule, different expression
	changedAt := base.Add(2 * time.Hour)
	r3 := &Rule{
		AlertName: "Test", GroupName: "g",
		Expr:      "up == 1",
		ExprHash:  hashB,
		FirstSeen: base, LastSeen: changedAt,
		Active: true,
	}
	_, _ = db.UpsertRule(ctx, r3)

	rules, _ = db.ListRules(ctx)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	if rules[0].ExprChangedAt.IsZero() {
		t.Error("expr_changed_at should be set when expression changes")
	}
	if !rules[0].ExprChangedAt.Equal(changedAt) {
		t.Errorf("expr_changed_at = %v, want %v", rules[0].ExprChangedAt, changedAt)
	}
}

// TestInsertEpisodesRejectsOverlappingEpisode is the store's own line of
// defence for issue #15. The collectors keep the sample grid stable so a
// rescan re-derives identical timestamps, but the grid depends on
// prometheus.step, which an operator can change between runs, and on the
// window start, which slides forward every run and re-clips any episode
// straddling it. Either produces the same firing at a shifted started_at,
// which the uniqueness key cannot recognise -- so the store rejects it on
// overlap instead. One series cannot be firing twice at once.
func TestInsertEpisodesRejectsOverlappingEpisode(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	start := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: start, LastSeen: start, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	mk := func(s, e time.Time) Episode {
		return Episode{
			RuleID: id, Fingerprint: "fp1", StartedAt: s, EndedAt: e,
			Resolution: time.Minute, Source: SourceBackfill, State: StateFiring,
		}
	}

	original := mk(start, start.Add(10*time.Minute))
	if err := db.InsertEpisodes(ctx, []Episode{original}); err != nil {
		t.Fatalf("InsertEpisodes: %v", err)
	}

	// The same firing re-observed on a grid shifted by 37s: a different
	// started_at, so a different uniqueness key, but plainly the same episode.
	shifted := mk(start.Add(37*time.Second), start.Add(10*time.Minute+37*time.Second))
	if err := db.InsertEpisodes(ctx, []Episode{shifted}); err != nil {
		t.Fatalf("InsertEpisodes shifted: %v", err)
	}

	got, err := db.ListEpisodesInWindow(ctx, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d episodes, want 1; an episode overlapping one already "+
			"stored for the same series is the same firing seen again", len(got))
	}
	if !got[0].StartedAt.Equal(original.StartedAt) || !got[0].EndedAt.Equal(original.EndedAt) {
		t.Errorf("stored episode = %v..%v, want the original %v..%v left alone",
			got[0].StartedAt, got[0].EndedAt, original.StartedAt, original.EndedAt)
	}
}

// TestInsertEpisodesKeepsNonOverlappingAndOtherSeries pins the limits of the
// reconciliation rule: it must reject re-observations of the same firing,
// never real history. A genuine later firing well beyond tolerance, and a
// same-instant episode of a different series or state, are all real and
// must survive.
func TestInsertEpisodesKeepsNonOverlappingAndOtherSeries(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	start := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: start, LastSeen: start, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	mk := func(fp, state string, s, e time.Time) Episode {
		return Episode{
			RuleID: id, Fingerprint: fp, StartedAt: s, EndedAt: e,
			Resolution: time.Minute, Source: SourceBackfill, State: state,
		}
	}

	first := mk("fp1", StateFiring, start, start.Add(10*time.Minute))
	if err := db.InsertEpisodes(ctx, []Episode{first}); err != nil {
		t.Fatalf("InsertEpisodes: %v", err)
	}

	rest := []Episode{
		// A genuine later re-fire of the same series, well beyond one
		// step's tolerance of the stored episode's end.
		mk("fp1", StateFiring, start.Add(30*time.Minute), start.Add(35*time.Minute)),
		// Same instant, different label set: a different series entirely.
		mk("fp2", StateFiring, start, start.Add(10*time.Minute)),
		// Same instant and series, but pending rather than firing. A rule is
		// routinely pending and firing over the same span.
		mk("fp1", StatePending, start, start.Add(10*time.Minute)),
	}
	if err := db.InsertEpisodes(ctx, rest); err != nil {
		t.Fatalf("InsertEpisodes rest: %v", err)
	}

	got, err := db.ListEpisodesInWindow(ctx, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
	}
	if len(got) != 1+len(rest) {
		t.Fatalf("got %d episodes, want %d; the overlap rule must reject "+
			"re-observations of one series, not real history", len(got), 1+len(rest))
	}
}

// TestInsertEpisodesReconcilesExactlyAdjacentEpisodes is finding 1 and 5
// applied to the backfill path's own self-consistency, not just its
// interaction with the webhook path: two backfill scans over a grid that
// shifted (an operator-changed prometheus.step, or a window boundary that
// slid forward) can produce two spans for one firing that touch with zero
// gap rather than overlap. That must reconcile into one episode, the same
// as it does for a backfill/webhook pair.
func TestInsertEpisodesReconcilesExactlyAdjacentEpisodes(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	start := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: start, LastSeen: start, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	mk := func(s, e time.Time) Episode {
		return Episode{
			RuleID: id, Fingerprint: "fp1", StartedAt: s, EndedAt: e,
			Resolution: time.Minute, Source: SourceBackfill, State: StateFiring,
		}
	}

	first := mk(start, start.Add(10*time.Minute))
	if err := db.InsertEpisodes(ctx, []Episode{first}); err != nil {
		t.Fatalf("InsertEpisodes: %v", err)
	}
	// Starts exactly where the first ends -- zero gap.
	touching := mk(start.Add(10*time.Minute), start.Add(15*time.Minute))
	if err := db.InsertEpisodes(ctx, []Episode{touching}); err != nil {
		t.Fatalf("InsertEpisodes touching: %v", err)
	}

	got, err := db.ListEpisodesInWindow(ctx, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d episodes, want 1; zero gap is adjacency, not a separate firing", len(got))
	}
}

// TestInsertEpisodesStillExtendsAnOngoingEpisode keeps the overlap rule from
// swallowing the upsert it sits in front of. A scan whose window ends mid
// firing records the episode short; the next scan sees the same started_at and
// a later end, and that update must still land.
func TestInsertEpisodesStillExtendsAnOngoingEpisode(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	start := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: start, LastSeen: start, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	mk := func(e time.Time) Episode {
		return Episode{
			RuleID: id, Fingerprint: "fp1", StartedAt: start, EndedAt: e,
			Resolution: time.Minute, Source: SourceBackfill, State: StateFiring,
		}
	}

	if err := db.InsertEpisodes(ctx, []Episode{mk(start.Add(5 * time.Minute))}); err != nil {
		t.Fatalf("InsertEpisodes: %v", err)
	}
	longer := start.Add(20 * time.Minute)
	if err := db.InsertEpisodes(ctx, []Episode{mk(longer)}); err != nil {
		t.Fatalf("re-InsertEpisodes: %v", err)
	}

	got, err := db.ListEpisodesInWindow(ctx, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d episodes, want 1", len(got))
	}
	if !got[0].EndedAt.Equal(longer) {
		t.Errorf("ended_at = %v, want %v; an episode still running at the end "+
			"of the last scan must be extendable", got[0].EndedAt, longer)
	}
}
