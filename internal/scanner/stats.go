package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

// ScanStats is a snapshot of one completed scan's operational numbers --
// the things an operator running noisefloor would want to alert on, per
// issue #12: how long it took, how much it reconstructed, how many rules
// it scored, whether Prometheus was flaky, and how the verdicts came out.
//
// It is computed by cmd/noisefloor's `scan` command (the only place that
// knows how long the run took end to end) and persisted via SaveStats, so
// `noisefloor serve` -- a separate, later process reading the same
// database -- can export it as the /metrics series a scan itself has no
// way to expose: nothing is scraping a batch command mid-run.
type ScanStats struct {
	// ComputedAt is when this scan started (cmd/noisefloor's `now`), which
	// is also what /metrics reports as noisefloor_last_scan_timestamp_seconds.
	ComputedAt      time.Time      `json:"computed_at"`
	DurationSeconds float64        `json:"duration_seconds"`
	Episodes        int            `json:"episodes_reconstructed"`
	RulesScored     int            `json:"rules_scored"`
	QueryFailures   int            `json:"query_failures"`
	Verdicts        map[string]int `json:"verdicts"`
}

// metaStore is the one thing SaveStats/LoadStats need from the store: a
// generic key/value slot. Narrowed so this package does not need the whole
// of store.SQLite to persist one JSON blob.
type metaStore interface {
	SetMeta(ctx context.Context, key, value string) error
	GetMeta(ctx context.Context, key string) (string, bool, error)
}

var _ metaStore = (*store.SQLite)(nil)

// scanStatsMetaKey is the meta-table key ScanStats is stored under. One
// scan's stats overwrite the last -- this is a "how did the most recent
// scan go" gauge source, not a history.
const scanStatsMetaKey = "scan_stats"

// SaveStats persists stats for the server to read later. A failure here
// (a locked database, a full disk) must not fail the scan that already
// succeeded and already wrote every score -- the caller logs it and moves
// on; see cmd/noisefloor's runScan.
func SaveStats(ctx context.Context, db metaStore, stats ScanStats) error {
	b, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshal scan stats: %w", err)
	}
	if err := db.SetMeta(ctx, scanStatsMetaKey, string(b)); err != nil {
		return fmt.Errorf("save scan stats: %w", err)
	}
	return nil
}

// LoadStats returns the most recently saved ScanStats. ok is false when no
// scan has ever completed against this database -- the server's /metrics
// handler then omits the scan-derived series entirely rather than
// reporting zeroes that look like a scan which found nothing.
func LoadStats(ctx context.Context, db metaStore) (ScanStats, bool, error) {
	raw, ok, err := db.GetMeta(ctx, scanStatsMetaKey)
	if err != nil {
		return ScanStats{}, false, fmt.Errorf("load scan stats: %w", err)
	}
	if !ok {
		return ScanStats{}, false, nil
	}
	var s ScanStats
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return ScanStats{}, false, fmt.Errorf("parse scan stats: %w", err)
	}
	return s, true, nil
}
