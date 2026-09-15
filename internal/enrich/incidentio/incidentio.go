// Package incidentio is an interface-conformance stub for issue #14's
// incident.io pager enricher.
//
// NOT IMPLEMENTED. This package has never made a request against a real or
// fixture incident.io API and ships no HTTP client at all. See
// internal/enrich/opsgenie's identical stub and package doc comment for
// the full reasoning -- this package exists for the same reason and
// follows the same contract: ErrNotImplemented on every call, no
// config.IncidentIO surface, and a plain, immediate refusal rather than a
// client that looks complete. See internal/enrich/pagerduty for the one
// provider actually implemented and README's "Pager enrichers" section for
// the honest summary of what is and isn't.
package incidentio

import (
	"context"
	"errors"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/enrich"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// ErrNotImplemented is returned by every call to (*Enricher).Enrich.
var ErrNotImplemented = errors.New(
	"incident.io enricher is not implemented: this is an interface-conformance " +
		"stub, not a tested integration -- see README's Pager enrichers section")

// Enricher satisfies enrich.Enricher without doing any real work.
type Enricher struct{}

func New() *Enricher { return &Enricher{} }

var _ enrich.Enricher = (*Enricher)(nil)

func (e *Enricher) Name() string { return "incident.io" }

// Enrich always returns ErrNotImplemented, immediately, without attempting
// any network call.
func (e *Enricher) Enrich(_ context.Context, _ store.Rule, _ []store.Episode, _, _ time.Time) (enrich.Result, error) {
	return enrich.Result{}, ErrNotImplemented
}
