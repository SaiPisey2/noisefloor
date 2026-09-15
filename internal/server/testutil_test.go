package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// newTestDB opens a fresh, temp-file-backed store -- store.Open runs the
// embedded schema, and a real file (rather than :memory:) matches how
// `noisefloor serve` actually opens its database.
func newTestDB(t *testing.T) *store.SQLite {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newTestServer builds a Server over db using config.Default()'s weights,
// with a fixed clock so episode-timeline and any "now"-relative rendering
// is deterministic across test runs.
func newTestServer(t *testing.T, db *store.SQLite) *Server {
	t.Helper()
	srv, err := New(config.Default(), db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.nowFunc = func() time.Time { return fixedNow }
	return srv
}

// fixedNow anchors every golden-file and timeline test to one instant, so
// re-running the suite never depends on the wall clock.
var fixedNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// seedRule upserts one rule and returns its ID.
func seedRule(t *testing.T, db *store.SQLite, r store.Rule) int64 {
	t.Helper()
	if r.FirstSeen.IsZero() {
		r.FirstSeen = fixedNow.Add(-60 * 24 * time.Hour)
	}
	if r.LastSeen.IsZero() {
		r.LastSeen = fixedNow
	}
	id, err := db.UpsertRule(context.Background(), &r)
	if err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	return id
}

// seedEpisode inserts one episode for ruleID.
func seedEpisode(t *testing.T, db *store.SQLite, ruleID int64, state string, start time.Time, dur time.Duration) {
	t.Helper()
	err := db.InsertEpisodes(context.Background(), []store.Episode{{
		RuleID: ruleID, Fingerprint: "fp-" + state + "-" + start.String(),
		Labels:    map[string]string{"instance": "test-1"},
		StartedAt: start, EndedAt: start.Add(dur),
		Resolution: time.Minute, Source: store.SourceBackfill, State: state,
	}})
	if err != nil {
		t.Fatalf("seed episode: %v", err)
	}
}

// seedScore upserts a score for ruleID.
func seedScore(t *testing.T, db *store.SQLite, ruleID int64, sc store.Score) {
	t.Helper()
	sc.RuleID = ruleID
	if sc.ComputedAt.IsZero() {
		sc.ComputedAt = fixedNow
	}
	if sc.WindowEnd.IsZero() {
		sc.WindowEnd = fixedNow
	}
	if sc.WindowStart.IsZero() {
		sc.WindowStart = fixedNow.Add(-30 * 24 * time.Hour)
	}
	if err := db.UpsertScore(context.Background(), &sc); err != nil {
		t.Fatalf("seed score: %v", err)
	}
}

// seedCoverageSnapshot saves snap to db under the fixed key coverage.Load
// reads.
func seedCoverageSnapshot(t *testing.T, db *store.SQLite, snap coverage.Snapshot) {
	t.Helper()
	if err := coverage.Save(context.Background(), db, snap); err != nil {
		t.Fatalf("seed coverage snapshot: %v", err)
	}
}
