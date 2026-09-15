package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedEpisodesForPaging inserts n non-overlapping one-minute firing
// episodes for ruleID, starting at base and spaced apart by step, each
// with its own fingerprint so InsertEpisodes' overlap guard never merges
// two of them into one.
func seedEpisodesForPaging(t *testing.T, db *SQLite, ruleID int64, n int, base time.Time, step time.Duration) {
	t.Helper()
	eps := make([]Episode, n)
	for i := 0; i < n; i++ {
		start := base.Add(time.Duration(i) * step)
		eps[i] = Episode{
			RuleID: ruleID, Fingerprint: fmt.Sprintf("fp-%d", i),
			StartedAt: start, EndedAt: start.Add(time.Minute),
			Resolution: time.Minute, Source: SourceBackfill, State: StateFiring,
		}
	}
	if err := db.InsertEpisodes(context.Background(), eps); err != nil {
		t.Fatalf("seed %d episodes: %v", n, err)
	}
}

func TestCountEpisodesForRule(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	seedEpisodesForPaging(t, db, id, 25, base, time.Hour)

	n, err := db.CountEpisodesForRule(ctx, id)
	if err != nil {
		t.Fatalf("CountEpisodesForRule: %v", err)
	}
	if n != 25 {
		t.Errorf("count = %d, want 25", n)
	}

	n, err = db.CountEpisodesForRule(ctx, id+999)
	if err != nil {
		t.Fatalf("CountEpisodesForRule (unknown rule): %v", err)
	}
	if n != 0 {
		t.Errorf("count for unknown rule = %d, want 0", n)
	}
}

func TestListEpisodesForRulePaginates(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	const total = 25
	seedEpisodesForPaging(t, db, id, total, base, time.Hour)

	page1, err := db.ListEpisodesForRule(ctx, id, 10, 0)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 10 {
		t.Fatalf("page1 len = %d, want 10", len(page1))
	}
	// Most recent first: the newest episode (index total-1) started latest.
	want := base.Add(time.Duration(total-1) * time.Hour)
	if !page1[0].StartedAt.Equal(want) {
		t.Errorf("page1[0].StartedAt = %v, want %v (most-recent-first order)", page1[0].StartedAt, want)
	}

	page2, err := db.ListEpisodesForRule(ctx, id, 10, 10)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 10 {
		t.Fatalf("page2 len = %d, want 10", len(page2))
	}
	page3, err := db.ListEpisodesForRule(ctx, id, 10, 20)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3) != 5 {
		t.Fatalf("page3 len = %d, want 5 (25 total, offset 20)", len(page3))
	}

	seen := map[int64]bool{}
	for _, e := range append(append(page1, page2...), page3...) {
		if seen[e.ID] {
			t.Fatalf("episode %d appeared on more than one page", e.ID)
		}
		seen[e.ID] = true
	}
	if len(seen) != total {
		t.Fatalf("pages covered %d distinct episodes, want %d", len(seen), total)
	}

	// limit <= 0 falls back to DefaultEpisodesPerPage, well above this
	// rule's 25 episodes, so it returns everything.
	all, err := db.ListEpisodesForRule(ctx, id, 0, 0)
	if err != nil {
		t.Fatalf("limit=0: %v", err)
	}
	if len(all) != total {
		t.Errorf("limit=0 returned %d episodes, want %d", len(all), total)
	}
}

// TestListEpisodesForRuleHardCap is the load-bearing assertion behind
// MaxEpisodesPerPage: no matter how large a limit a caller asks for (a
// malicious or merely careless `?limit=`), one call never returns more
// than the hard cap -- the fix for the unbounded query that let a single
// request to a flapping rule allocate tens of megabytes.
func TestListEpisodesForRuleHardCap(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "Flapper", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	seedEpisodesForPaging(t, db, id, MaxEpisodesPerPage+50, base, time.Minute)

	got, err := db.ListEpisodesForRule(ctx, id, MaxEpisodesPerPage+1000, 0)
	if err != nil {
		t.Fatalf("ListEpisodesForRule: %v", err)
	}
	if len(got) != MaxEpisodesPerPage {
		t.Fatalf("got %d episodes, want the hard cap of %d even though more exist and a larger limit was requested",
			len(got), MaxEpisodesPerPage)
	}
}

func TestListFiringEpisodesForRuleInWindow(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "A", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	windowStart := base
	windowEnd := base.Add(24 * time.Hour)

	inWindowFiring := Episode{
		RuleID: id, Fingerprint: "in-firing", StartedAt: base.Add(time.Hour), EndedAt: base.Add(time.Hour + time.Minute),
		Resolution: time.Minute, Source: SourceBackfill, State: StateFiring,
	}
	inWindowPending := Episode{
		RuleID: id, Fingerprint: "in-pending", StartedAt: base.Add(2 * time.Hour), EndedAt: base.Add(2*time.Hour + time.Minute),
		Resolution: time.Minute, Source: SourceBackfill, State: StatePending,
	}
	// Ninety days old -- outside the window, the exact shape of the
	// must-fix-1 repro (stale long episodes that must not leak into a
	// window-scoped counterfactual).
	outsideOld := Episode{
		RuleID: id, Fingerprint: "old-firing", StartedAt: base.Add(-90 * 24 * time.Hour), EndedAt: base.Add(-90*24*time.Hour + time.Hour),
		Resolution: time.Minute, Source: SourceBackfill, State: StateFiring,
	}
	outsideAfter := Episode{
		RuleID: id, Fingerprint: "after-firing", StartedAt: windowEnd.Add(time.Hour), EndedAt: windowEnd.Add(2 * time.Hour),
		Resolution: time.Minute, Source: SourceBackfill, State: StateFiring,
	}
	if err := db.InsertEpisodes(ctx, []Episode{inWindowFiring, inWindowPending, outsideOld, outsideAfter}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := db.ListFiringEpisodesForRuleInWindow(ctx, id, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("ListFiringEpisodesForRuleInWindow: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d episodes, want 1 (only the in-window firing one)", len(got))
	}
	if got[0].Fingerprint != "in-firing" {
		t.Errorf("got fingerprint %q, want in-firing", got[0].Fingerprint)
	}
}

func TestEpisodeTimelineSummary(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "Flapper", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	// 1000 episodes, one per minute, alternating firing/pending -- spans
	// almost 16.7 hours (999 minutes) from the first episode's start to
	// the last one's end.
	const n = 1000
	eps := make([]Episode, n)
	for i := 0; i < n; i++ {
		start := base.Add(time.Duration(i) * time.Minute)
		state := StateFiring
		if i%2 == 1 {
			state = StatePending
		}
		eps[i] = Episode{
			RuleID: id, Fingerprint: fmt.Sprintf("fp-%d", i),
			StartedAt: start, EndedAt: start.Add(30 * time.Second),
			Resolution: time.Minute, Source: SourceBackfill, State: state,
		}
	}
	if err := db.InsertEpisodes(ctx, eps); err != nil {
		t.Fatalf("seed: %v", err)
	}

	now := base.Add(48 * time.Hour) // well after the last episode ended
	buckets, bucketWidth, windowStart, windowEnd, total, err := db.EpisodeTimelineSummary(ctx, id, now, 10)
	if err != nil {
		t.Fatalf("EpisodeTimelineSummary: %v", err)
	}
	if total != n {
		t.Fatalf("total = %d, want %d", total, n)
	}
	if !windowStart.Equal(base) {
		t.Errorf("windowStart = %v, want %v (first episode's start)", windowStart, base)
	}
	if !windowEnd.Equal(now) {
		t.Errorf("windowEnd = %v, want %v (now, since it is after the last episode ended)", windowEnd, now)
	}
	if bucketWidth <= 0 {
		t.Fatalf("bucketWidth = %v, want positive", bucketWidth)
	}

	var firingSum, pendingSum int
	for _, b := range buckets {
		firingSum += b.Firing
		pendingSum += b.Pending
	}
	if firingSum+pendingSum != n {
		t.Errorf("buckets account for %d+%d=%d episodes, want %d", firingSum, pendingSum, firingSum+pendingSum, n)
	}
	if firingSum != n/2 || pendingSum != n/2 {
		t.Errorf("firing/pending split = %d/%d, want %d/%d", firingSum, pendingSum, n/2, n/2)
	}
	// Every episode started inside [windowStart, windowEnd), so every
	// bucket index a row lands in must be non-negative and within
	// numBuckets's ballpark -- generous headroom since windowEnd was
	// extended out to `now`, which widens bucketWidth and pushes the
	// occupied index range down, not up.
	for _, b := range buckets {
		if b.Index < 0 {
			t.Errorf("bucket index %d is negative", b.Index)
		}
	}
}

func TestEpisodeTimelineSummaryNoEpisodes(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	base := time.Unix(1_700_000_000, 0).UTC()

	id, err := db.UpsertRule(ctx, &Rule{AlertName: "Quiet", GroupName: "g", FirstSeen: base, LastSeen: base, Active: true})
	if err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	buckets, bucketWidth, windowStart, windowEnd, total, err := db.EpisodeTimelineSummary(ctx, id, base, 10)
	if err != nil {
		t.Fatalf("EpisodeTimelineSummary: %v", err)
	}
	if total != 0 || len(buckets) != 0 || bucketWidth != 0 || !windowStart.IsZero() || !windowEnd.IsZero() {
		t.Errorf("expected all-zero result for a rule with no episodes, got buckets=%v bucketWidth=%v "+
			"windowStart=%v windowEnd=%v total=%d", buckets, bucketWidth, windowStart, windowEnd, total)
	}
}

func TestOpenSetsMaxOpenConns(t *testing.T) {
	db := openTest(t)
	if got := db.db.Stats().MaxOpenConnections; got != maxOpenConns {
		t.Errorf("MaxOpenConnections = %d, want %d -- an unbounded pool lets a burst of "+
			"concurrent requests each open a connection with no limit", got, maxOpenConns)
	}
}
