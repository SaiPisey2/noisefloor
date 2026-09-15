// Package opsgenie is an interface-conformance stub for issue #14's
// Opsgenie pager enricher.
//
// NOT IMPLEMENTED. This package has never made a request against a real or
// fixture Opsgenie API and ships no HTTP client at all. It exists so
// enrich.Enricher has a second implementation proving the interface holds
// for a provider PagerDuty's own shapes did not inform, and so a caller
// who configures Opsgenie gets an explicit, immediate refusal -- see
// ErrNotImplemented -- instead of a client that LOOKS complete but was
// never exercised against anything. See the README's "Pager enrichers"
// section and internal/enrich/pagerduty's package doc comment for the
// implemented reference.
//
// There is deliberately no config.Opsgenie: adding a config surface for an
// integration that cannot be used would let an operator "configure" it and
// discover the gap only at scan time. Whoever implements this next should
// add both together.
package opsgenie

import (
	"context"
	"errors"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/enrich"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// ErrNotImplemented is returned by every call to (*Enricher).Enrich. A
// scan that somehow ends up with this enricher wired in degrades to
// estimated scoring with this error printed as a warning -- the same
// posture internal/scanner already has for an unreachable PagerDuty or
// Alertmanager.
var ErrNotImplemented = errors.New(
	"opsgenie enricher is not implemented: this is an interface-conformance " +
		"stub, not a tested integration -- see README's Pager enrichers section")

// Enricher satisfies enrich.Enricher without doing any real work. See the
// package doc comment.
type Enricher struct{}

// New constructs the stub. Present for symmetry with
// internal/enrich/pagerduty.New, and so a future implementation's
// signature change is a smaller diff.
func New() *Enricher { return &Enricher{} }

var _ enrich.Enricher = (*Enricher)(nil)

func (e *Enricher) Name() string { return "opsgenie" }

// Enrich always returns ErrNotImplemented, immediately, without attempting
// any network call.
func (e *Enricher) Enrich(_ context.Context, _ store.Rule, _ []store.Episode, _, _ time.Time) (enrich.Result, error) {
	return enrich.Result{}, ErrNotImplemented
}
