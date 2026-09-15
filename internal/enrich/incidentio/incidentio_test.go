package incidentio

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

func TestNameIsIncidentIO(t *testing.T) {
	if got := New().Name(); got != "incident.io" {
		t.Errorf("Name() = %q, want %q", got, "incident.io")
	}
}

func TestEnrichReturnsNotImplemented(t *testing.T) {
	_, err := New().Enrich(context.Background(), store.Rule{}, nil, time.Time{}, time.Time{})
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("err = %v, want ErrNotImplemented", err)
	}
}
