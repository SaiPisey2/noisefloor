package coverage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Snapshot is a JSON-serialisable copy of one Run's Result, persisted so
// the read-only server (issue #12) can render the coverage grid and
// blind-spot list without ever issuing a live Prometheus query itself --
// an unauthenticated page load must not be able to trigger the range and
// instant queries Run performs. `noisefloor coverage` calls Save after
// every run; the server calls Load and renders whatever it finds.
//
// Result itself is not JSON-safe: ParseErrors is a map to the error
// interface, which json.Marshal reduces to "{}" and cannot round-trip.
// Snapshot substitutes each error's message.
type Snapshot struct {
	ComputedAt   time.Time         `json:"computed_at"`
	Grid         []ServiceCoverage `json:"grid"`
	ParseErrors  map[string]string `json:"parse_errors"`
	Unattributed []string          `json:"unattributed"`
	ExcludedJobs []string          `json:"excluded_jobs"`
}

// NewSnapshot converts a Run result into its persisted form.
func NewSnapshot(now time.Time, r Result) Snapshot {
	parseErrors := make(map[string]string, len(r.ParseErrors))
	for k, err := range r.ParseErrors {
		parseErrors[k] = err.Error()
	}
	return Snapshot{
		ComputedAt:   now,
		Grid:         r.Grid,
		ParseErrors:  parseErrors,
		Unattributed: r.Unattributed,
		ExcludedJobs: r.ExcludedJobs,
	}
}

// metaStore is the one thing Save/Load need from the store: a generic
// key/value slot. Narrowed so this package need not import internal/store
// at all -- it already stands apart from how episodes are fetched (see
// this package's doc comment), and a coverage.Snapshot has nothing to do
// with rules or episodes.
type metaStore interface {
	SetMeta(ctx context.Context, key, value string) error
	GetMeta(ctx context.Context, key string) (string, bool, error)
}

// coverageSnapshotMetaKey is the meta-table key a Snapshot is stored
// under. One coverage run's result overwrites the last: the grid answers
// "what does coverage look like right now", not a history of past runs.
const coverageSnapshotMetaKey = "coverage_snapshot"

// Save persists snap for the server to read later.
func Save(ctx context.Context, db metaStore, snap Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal coverage snapshot: %w", err)
	}
	if err := db.SetMeta(ctx, coverageSnapshotMetaKey, string(b)); err != nil {
		return fmt.Errorf("save coverage snapshot: %w", err)
	}
	return nil
}

// Load returns the most recently saved Snapshot. ok is false when
// `noisefloor coverage` has never run against this database.
func Load(ctx context.Context, db metaStore) (Snapshot, bool, error) {
	raw, ok, err := db.GetMeta(ctx, coverageSnapshotMetaKey)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("load coverage snapshot: %w", err)
	}
	if !ok {
		return Snapshot{}, false, nil
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return Snapshot{}, false, fmt.Errorf("parse coverage snapshot: %w", err)
	}
	return snap, true, nil
}
