package opsgenie

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

func TestNameIsOpsgenie(t *testing.T) {
	if got := New().Name(); got != "opsgenie" {
		t.Errorf("Name() = %q, want %q", got, "opsgenie")
	}
}

// TestEnrichReturnsNotImplemented pins the honest-gap contract: this
// integration must refuse plainly rather than return an empty, successful
// result that could be mistaken for "checked, found nothing".
func TestEnrichReturnsNotImplemented(t *testing.T) {
	_, err := New().Enrich(context.Background(), store.Rule{}, nil, time.Time{}, time.Time{})
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("err = %v, want ErrNotImplemented", err)
	}
}
