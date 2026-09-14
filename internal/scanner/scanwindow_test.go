package scanner

import (
	"testing"
	"time"
)

// TestScanWindowIsIdenticalWithinAStep is the alignment half of issue #15:
// every instant inside one step must produce the same window, because the
// window start decides the sample grid Prometheus returns and therefore the
// started_at of every episode the scan stores.
func TestScanWindowIsIdenticalWithinAStep(t *testing.T) {
	step := time.Minute
	window := 30 * 24 * time.Hour
	base := time.Unix(1_700_000_000, 0).UTC().Truncate(step)

	wantFrom, wantTo := ScanWindow(base, window, step)
	if !wantTo.Equal(base) {
		t.Fatalf("to = %v for an already-aligned instant, want %v unchanged", wantTo, base)
	}
	if !wantFrom.Equal(base.Add(-window)) {
		t.Fatalf("from = %v, want %v (to minus the window)", wantFrom, base.Add(-window))
	}

	for _, offset := range []time.Duration{
		time.Nanosecond, time.Second, 17 * time.Second,
		30 * time.Second, step - time.Nanosecond,
	} {
		from, to := ScanWindow(base.Add(offset), window, step)
		if !from.Equal(wantFrom) || !to.Equal(wantTo) {
			t.Errorf("ScanWindow(base+%v) = %v..%v, want %v..%v; scans within one "+
				"step must ask for the same window or the sample grid shifts and "+
				"the same episodes get stored twice",
				offset, from, to, wantFrom, wantTo)
		}
	}
}

// TestScanWindowAdvancesByAStep is the other side: the window is anchored, not
// frozen. Crossing into the next step moves it by exactly one step, so a scan
// still sees history accumulating.
func TestScanWindowAdvancesByAStep(t *testing.T) {
	step := time.Minute
	window := 24 * time.Hour
	base := time.Unix(1_700_000_000, 0).UTC().Truncate(step)

	_, to := ScanWindow(base, window, step)
	fromNext, toNext := ScanWindow(base.Add(step), window, step)

	if got := toNext.Sub(to); got != step {
		t.Errorf("window end advanced by %v across a step boundary, want %v", got, step)
	}
	if got := toNext.Sub(fromNext); got != window {
		t.Errorf("window width = %v, want the configured %v", got, window)
	}
}

// TestScanWindowLandsOnTheStepGrid pins the grid itself. Truncate rounds down
// against absolute time since the zero instant, which is what makes the window
// reproducible across processes and machines rather than merely stable within
// one run.
func TestScanWindowLandsOnTheStepGrid(t *testing.T) {
	for _, step := range []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute, time.Hour} {
		now := time.Unix(1_700_000_037, 123_456_789).UTC()
		_, to := ScanWindow(now, 24*time.Hour, step)
		if !to.Equal(to.Truncate(step)) {
			t.Errorf("step %v: to = %v is not on the grid", step, to)
		}
		if to.After(now) || now.Sub(to) >= step {
			t.Errorf("step %v: to = %v, want the grid point at or just below %v",
				step, to, now)
		}
	}
}

// TestScanWindowToleratesNonPositiveStep guards the degenerate config that
// Validate rejects but a future caller might not: no step grid to snap to
// still has to yield the requested window, not a zero one.
func TestScanWindowToleratesNonPositiveStep(t *testing.T) {
	now := time.Unix(1_700_000_037, 0).UTC()
	window := time.Hour

	for _, step := range []time.Duration{0, -time.Minute} {
		from, to := ScanWindow(now, window, step)
		if !to.Equal(now) || !from.Equal(now.Add(-window)) {
			t.Errorf("step %v: window = %v..%v, want the unsnapped %v..%v",
				step, from, to, now.Add(-window), now)
		}
	}
}
