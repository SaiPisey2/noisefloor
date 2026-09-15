package store

import (
	"context"
	"testing"
	"time"
)

// seedWebhookRule upserts a minimal active rule and returns its ID.
func seedWebhookRule(t *testing.T, db *SQLite, alertName string, now time.Time) int64 {
	t.Helper()
	id, err := db.UpsertRule(context.Background(), &Rule{
		AlertName: alertName, GroupName: "g", FirstSeen: now, LastSeen: now, Active: true,
	})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	return id
}

func countEpisodesForFingerprint(t *testing.T, db *SQLite, ruleID int64, fp string) int {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(),
		`SELECT id FROM episodes WHERE rule_id = ? AND fingerprint = ?`, ruleID, fp)
	if err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n
}

// TestBackfillAfterWebhookDoesNotDoubleCount and
// TestWebhookAfterBackfillDoesNotDoubleCount are finding 1: InsertEpisodes
// (the backfill path) used to reject only strict overlap, while
// UpsertWebhookEpisode (the webhook path) already reconciled on
// overlap-or-adjacent-within-tolerance. An alert firing at 10:00:05 and
// notified at 10:00:30 stores a webhook episode [10:00:05, 10:00:30]; the
// backfill later stores [10:01:00, 10:05:00] from the sample grid -- no
// overlap, but well within one step's tolerance, and the same firing.
// Both write orders must converge on exactly one stored episode.
func TestBackfillAfterWebhookDoesNotDoubleCount(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "Outlives", base)

	webhookEp := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: base.Add(5 * time.Second), EndedAt: base.Add(30 * time.Second),
		Source: SourceWebhook, State: StateFiring,
	}
	if _, reconciled, err := db.UpsertWebhookEpisode(ctx, webhookEp, WebhookMeta{Receiver: "pager"}); err != nil {
		t.Fatalf("UpsertWebhookEpisode: %v", err)
	} else if reconciled {
		t.Fatalf("reconciled = true, want false: nothing stored yet")
	}

	backfilled := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: base.Add(60 * time.Second), EndedAt: base.Add(5 * time.Minute),
		Resolution: time.Minute, Source: SourceBackfill, State: StateFiring,
	}
	if err := db.InsertEpisodes(ctx, []Episode{backfilled}); err != nil {
		t.Fatalf("InsertEpisodes: %v", err)
	}

	if n := countEpisodesForFingerprint(t, db, ruleID, "fp1"); n != 1 {
		t.Fatalf("episodes for fp1 = %d, want 1: the backfill's grid-quantized "+
			"start is within one step of the webhook's notified end, the same firing", n)
	}
}

func TestWebhookAfterBackfillDoesNotDoubleCount(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "OutlivesReverse", base)

	backfilled := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: base.Add(60 * time.Second), EndedAt: base.Add(5 * time.Minute),
		Resolution: time.Minute, Source: SourceBackfill, State: StateFiring,
	}
	if err := db.InsertEpisodes(ctx, []Episode{backfilled}); err != nil {
		t.Fatalf("InsertEpisodes: %v", err)
	}

	webhookEp := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: base.Add(5 * time.Second), EndedAt: base.Add(30 * time.Second),
		Source: SourceWebhook, State: StateFiring,
	}
	if _, reconciled, err := db.UpsertWebhookEpisode(ctx, webhookEp, WebhookMeta{Receiver: "pager"}); err != nil {
		t.Fatalf("UpsertWebhookEpisode: %v", err)
	} else if !reconciled {
		t.Errorf("reconciled = false, want true: within tolerance of the backfilled episode")
	}

	if n := countEpisodesForFingerprint(t, db, ruleID, "fp1"); n != 1 {
		t.Fatalf("episodes for fp1 = %d, want 1: the webhook's notified end is "+
			"within one step of the backfill's grid-quantized start, the same firing", n)
	}
}

// TestUpsertWebhookEpisodeNoExistingRowInsertsNew covers the "genuinely new"
// case: no backfilled episode explains this window at all -- a firing
// invisible to the backfill because it started and resolved inside one
// query step.
func TestUpsertWebhookEpisodeNoExistingRowInsertsNew(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "Sub", now)

	ep := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(30 * time.Second),
		Source: SourceWebhook, State: StateFiring,
	}
	id, reconciled, err := db.UpsertWebhookEpisode(ctx, ep, WebhookMeta{Receiver: "pager"})
	if err != nil {
		t.Fatalf("UpsertWebhookEpisode: %v", err)
	}
	if reconciled {
		t.Errorf("reconciled = true, want false for a genuinely new episode")
	}
	if id == 0 {
		t.Fatalf("got id 0")
	}
	if n := countEpisodesForFingerprint(t, db, ruleID, "fp1"); n != 1 {
		t.Fatalf("episodes for fp1 = %d, want 1", n)
	}

	meta, ok, err := db.GetWebhookMeta(ctx, id)
	if err != nil {
		t.Fatalf("GetWebhookMeta: %v", err)
	}
	if !ok {
		t.Fatalf("no webhook meta recorded for new episode")
	}
	if meta.Receiver != "pager" {
		t.Errorf("Receiver = %q, want pager", meta.Receiver)
	}
}

// TestUpsertWebhookEpisodeOverlappingBackfillReconciles is the core
// reconciliation case: a backfilled episode already covers this firing at
// step resolution, and the webhook's exact span sits inside it. This must
// become one episode, with the backfill's boundaries left untouched and the
// webhook's metadata attached.
func TestUpsertWebhookEpisodeOverlappingBackfillReconciles(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "Both", now)
	step := time.Minute

	backfilled := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(step),
		Resolution: step, Source: SourceBackfill, State: StateFiring,
	}
	if err := db.InsertEpisodes(ctx, []Episode{backfilled}); err != nil {
		t.Fatalf("seed backfill episode: %v", err)
	}

	// The webhook's exact span (10s..40s) is fully inside the backfilled
	// episode's coarser [0, step) span.
	webhookEp := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now.Add(10 * time.Second), EndedAt: now.Add(40 * time.Second),
		Source: SourceWebhook, State: StateFiring,
	}
	meta := WebhookMeta{
		Receiver: "pager", GeneratorURL: "http://prom/graph",
		PreciseStartedAt: webhookEp.StartedAt, PreciseEndedAt: webhookEp.EndedAt,
	}
	id, reconciled, err := db.UpsertWebhookEpisode(ctx, webhookEp, meta)
	if err != nil {
		t.Fatalf("UpsertWebhookEpisode: %v", err)
	}
	if !reconciled {
		t.Errorf("reconciled = false, want true for an overlapping backfilled episode")
	}
	if n := countEpisodesForFingerprint(t, db, ruleID, "fp1"); n != 1 {
		t.Fatalf("episodes for fp1 = %d, want exactly 1 (double-counted)", n)
	}

	eps, err := db.ListEpisodesInWindow(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d episodes, want 1", len(eps))
	}
	got := eps[0]
	if !got.StartedAt.Equal(now) || !got.EndedAt.Equal(now.Add(step)) {
		t.Errorf("boundaries changed: got [%s, %s), want the backfill's unchanged [%s, %s)",
			got.StartedAt, got.EndedAt, now, now.Add(step))
	}
	if got.Source != SourceBackfill {
		t.Errorf("Source = %q, want unchanged %q", got.Source, SourceBackfill)
	}

	wm, ok, err := db.GetWebhookMeta(ctx, id)
	if err != nil || !ok {
		t.Fatalf("GetWebhookMeta: ok=%v err=%v", ok, err)
	}
	if wm.Receiver != "pager" {
		t.Errorf("Receiver = %q, want pager", wm.Receiver)
	}
	if !wm.PreciseStartedAt.Equal(webhookEp.StartedAt) {
		t.Errorf("PreciseStartedAt = %s, want the webhook's exact %s", wm.PreciseStartedAt, webhookEp.StartedAt)
	}
}

// TestUpsertWebhookEpisodeAdjacentToBackfillReconciles: the webhook's span
// does not overlap the backfilled episode at all, but sits within one
// resolution step of it -- exactly the imprecision BuildIntervals documents
// (an episode's ended_at overestimates by at most one step). This must
// still reconcile to one episode, not create a second.
func TestUpsertWebhookEpisodeAdjacentToBackfillReconciles(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "Adjacent", now)
	step := time.Minute

	backfilled := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(step),
		Resolution: step, Source: SourceBackfill, State: StateFiring,
	}
	if err := db.InsertEpisodes(ctx, []Episode{backfilled}); err != nil {
		t.Fatalf("seed backfill episode: %v", err)
	}

	// Starts 30s after the backfilled episode's ended_at -- inside one
	// step's tolerance, so still the same firing.
	webhookEp := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now.Add(step + 30*time.Second), EndedAt: now.Add(step + 45*time.Second),
		Source: SourceWebhook, State: StateFiring,
	}
	_, reconciled, err := db.UpsertWebhookEpisode(ctx, webhookEp, WebhookMeta{Receiver: "pager"})
	if err != nil {
		t.Fatalf("UpsertWebhookEpisode: %v", err)
	}
	if !reconciled {
		t.Errorf("reconciled = false, want true for an episode adjacent within one step")
	}
	if n := countEpisodesForFingerprint(t, db, ruleID, "fp1"); n != 1 {
		t.Fatalf("episodes for fp1 = %d, want 1", n)
	}
}

// TestUpsertWebhookEpisodeExactlyAdjacentReconciles is finding 5: the gap
// computation in findReconcilableEpisode required gap > 0, excluding the
// gap == 0 case -- exact adjacency, touching with no gap at all -- which is
// the definitional case the doc comment claims to cover ("overlapping or
// adjacent within the wider resolution") and exactly what a webhook
// startsAt truncated to the second produces when it lands precisely on a
// backfilled episode's ended_at.
func TestUpsertWebhookEpisodeExactlyAdjacentReconciles(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "Touching", now)
	step := time.Minute

	backfilled := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(step),
		Resolution: step, Source: SourceBackfill, State: StateFiring,
	}
	if err := db.InsertEpisodes(ctx, []Episode{backfilled}); err != nil {
		t.Fatalf("seed backfill episode: %v", err)
	}

	// Starts exactly where the backfilled episode ends -- zero gap, not a
	// few seconds inside tolerance.
	webhookEp := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now.Add(step), EndedAt: now.Add(step + 15*time.Second),
		Source: SourceWebhook, State: StateFiring,
	}
	_, reconciled, err := db.UpsertWebhookEpisode(ctx, webhookEp, WebhookMeta{Receiver: "pager"})
	if err != nil {
		t.Fatalf("UpsertWebhookEpisode: %v", err)
	}
	if !reconciled {
		t.Errorf("reconciled = false, want true: zero gap is adjacency, not a separate firing")
	}
	if n := countEpisodesForFingerprint(t, db, ruleID, "fp1"); n != 1 {
		t.Fatalf("episodes for fp1 = %d, want 1", n)
	}
}

// TestUpsertWebhookEpisodeBeyondToleranceInsertsNew is the control for the
// adjacency test above: a gap well beyond one step is a genuinely separate
// firing and must NOT be merged into the earlier episode.
func TestUpsertWebhookEpisodeBeyondToleranceInsertsNew(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "Separate", now)
	step := time.Minute

	backfilled := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(step),
		Resolution: step, Source: SourceBackfill, State: StateFiring,
	}
	if err := db.InsertEpisodes(ctx, []Episode{backfilled}); err != nil {
		t.Fatalf("seed backfill episode: %v", err)
	}

	webhookEp := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now.Add(time.Hour), EndedAt: now.Add(time.Hour + 30*time.Second),
		Source: SourceWebhook, State: StateFiring,
	}
	_, reconciled, err := db.UpsertWebhookEpisode(ctx, webhookEp, WebhookMeta{Receiver: "pager"})
	if err != nil {
		t.Fatalf("UpsertWebhookEpisode: %v", err)
	}
	if reconciled {
		t.Errorf("reconciled = true, want false: this firing is an hour later, not the same one")
	}
	if n := countEpisodesForFingerprint(t, db, ruleID, "fp1"); n != 2 {
		t.Fatalf("episodes for fp1 = %d, want 2 (two genuinely separate firings)", n)
	}
}

// TestUpsertWebhookEpisodeIdenticalBoundariesMerges: both sources happen to
// agree on the exact started_at (the common case for a repeat webhook
// notification of an episode the webhook itself already recorded). This
// must extend, not duplicate, and must never shrink ended_at.
func TestUpsertWebhookEpisodeIdenticalBoundariesMerges(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "Repeat", now)

	first := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(time.Minute),
		Source: SourceWebhook, State: StateFiring,
	}
	id1, reconciled1, err := db.UpsertWebhookEpisode(ctx, first, WebhookMeta{Receiver: "pager"})
	if err != nil {
		t.Fatalf("UpsertWebhookEpisode (first): %v", err)
	}
	if reconciled1 {
		t.Errorf("first notification: reconciled = true, want false")
	}

	// A repeat notification for the SAME alert: identical startsAt, later
	// endsAt (Alertmanager's repeat_interval firing it again while it's
	// still going).
	repeat := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(5 * time.Minute),
		Source: SourceWebhook, State: StateFiring,
	}
	id2, reconciled2, err := db.UpsertWebhookEpisode(ctx, repeat, WebhookMeta{Receiver: "pager"})
	if err != nil {
		t.Fatalf("UpsertWebhookEpisode (repeat): %v", err)
	}
	if !reconciled2 {
		t.Errorf("repeat notification: reconciled = false, want true")
	}
	if id1 != id2 {
		t.Errorf("repeat notification created a new row: id1=%d id2=%d", id1, id2)
	}
	if n := countEpisodesForFingerprint(t, db, ruleID, "fp1"); n != 1 {
		t.Fatalf("episodes for fp1 = %d, want 1", n)
	}

	eps, err := db.ListEpisodesInWindow(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
	}
	if len(eps) != 1 || !eps[0].EndedAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("episode not extended by repeat notification: %+v", eps)
	}

	// A resolved notification with an EARLIER endsAt than the last repeat's
	// placeholder must never shrink the stored ended_at.
	resolved := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(2 * time.Minute),
		Source: SourceWebhook, State: StateFiring,
	}
	if _, _, err := db.UpsertWebhookEpisode(ctx, resolved, WebhookMeta{Receiver: "pager"}); err != nil {
		t.Fatalf("UpsertWebhookEpisode (resolved): %v", err)
	}
	eps, err = db.ListEpisodesInWindow(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
	}
	if len(eps) != 1 || !eps[0].EndedAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("ended_at shrank: %+v", eps)
	}
}

// TestUpsertWebhookEpisodeIdenticalToBackfillPromotesSource covers the
// exact-boundary coincidence between the two sources: source is promoted
// to webhook (richer data is now available for this firing) but the
// (larger, conservative) backfilled ended_at is preserved.
func TestUpsertWebhookEpisodeIdenticalToBackfillPromotesSource(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "Identical", now)
	step := time.Minute

	backfilled := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(step),
		Resolution: step, Source: SourceBackfill, State: StateFiring,
	}
	if err := db.InsertEpisodes(ctx, []Episode{backfilled}); err != nil {
		t.Fatalf("seed backfill episode: %v", err)
	}

	webhookEp := Episode{
		RuleID: ruleID, Fingerprint: "fp1",
		StartedAt: now, EndedAt: now.Add(20 * time.Second), // shorter, exact
		Source: SourceWebhook, State: StateFiring,
	}
	id, reconciled, err := db.UpsertWebhookEpisode(ctx, webhookEp, WebhookMeta{Receiver: "pager"})
	if err != nil {
		t.Fatalf("UpsertWebhookEpisode: %v", err)
	}
	if !reconciled {
		t.Errorf("reconciled = false, want true")
	}
	eps, err := db.ListEpisodesInWindow(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d episodes, want 1", len(eps))
	}
	if eps[0].ID != id {
		t.Fatalf("unexpected id mismatch")
	}
	if eps[0].Source != SourceWebhook {
		t.Errorf("Source = %q, want promoted to %q", eps[0].Source, SourceWebhook)
	}
	// ended_at must never shrink: the backfill's coarser (later) end wins.
	if !eps[0].EndedAt.Equal(now.Add(step)) {
		t.Errorf("EndedAt = %s, want unchanged (never-shrink) %s", eps[0].EndedAt, now.Add(step))
	}
}

// TestUpsertWebhookEpisodeDifferentFingerprintsNeverMerge is the negative
// control for the whole reconciliation rule: two different labelsets of
// the same rule, firing at the same time, must remain two episodes.
func TestUpsertWebhookEpisodeDifferentFingerprintsNeverMerge(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	ruleID := seedWebhookRule(t, db, "MultiInstance", now)

	a := Episode{RuleID: ruleID, Fingerprint: "fp-a", StartedAt: now, EndedAt: now.Add(time.Minute), Source: SourceWebhook, State: StateFiring}
	b := Episode{RuleID: ruleID, Fingerprint: "fp-b", StartedAt: now, EndedAt: now.Add(time.Minute), Source: SourceWebhook, State: StateFiring}

	if _, _, err := db.UpsertWebhookEpisode(ctx, a, WebhookMeta{}); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if _, _, err := db.UpsertWebhookEpisode(ctx, b, WebhookMeta{}); err != nil {
		t.Fatalf("insert b: %v", err)
	}

	eps, err := db.ListEpisodesInWindow(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEpisodesInWindow: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("got %d episodes, want 2 (different fingerprints must not merge)", len(eps))
	}
}
