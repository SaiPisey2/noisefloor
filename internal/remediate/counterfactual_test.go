package remediate

import (
	"testing"
	"time"
)

func m(minutes float64) time.Duration { return time.Duration(minutes * float64(time.Minute)) }

func TestCompute(t *testing.T) {
	tests := []struct {
		name          string
		durations     []time.Duration // firing durations only, excluding for:
		currentFor    time.Duration
		candidateFor  time.Duration
		longThreshold time.Duration

		wantSuppressed   int
		wantRetained     int
		wantDelay        time.Duration
		wantLongTotal    int
		wantLongRetained int
	}{
		{
			// All episodes identical: underlying = 1m+3m = 4m for every one,
			// which is < candidate 5m, so every episode is suppressed.
			name:          "all episodes identical, all suppressed",
			durations:     []time.Duration{m(3), m(3), m(3), m(3), m(3)},
			currentFor:    m(1),
			candidateFor:  m(5),
			longThreshold: m(10),

			wantSuppressed:   5,
			wantRetained:     0,
			wantDelay:        m(4),
			wantLongTotal:    0,
			wantLongRetained: 0,
		},
		{
			// Single episode: underlying = 1m+12m = 13m >= candidate 5m,
			// retained. Its firing duration (12m) clears the 10m long bar.
			name:          "single episode, retained and long",
			durations:     []time.Duration{m(12)},
			currentFor:    m(1),
			candidateFor:  m(5),
			longThreshold: m(10),

			wantSuppressed:   0,
			wantRetained:     1,
			wantDelay:        m(4),
			wantLongTotal:    1,
			wantLongRetained: 1,
		},
		{
			// Exactly at the candidate boundary: underlying = 1m+4m = 5m,
			// which equals candidateFor exactly. Suppression is strict
			// less-than (Prometheus fires once the condition has held for
			// AT LEAST for:), so this is retained, not suppressed.
			name:          "episode exactly at candidate boundary is retained",
			durations:     []time.Duration{m(4)},
			currentFor:    m(1),
			candidateFor:  m(5),
			longThreshold: m(10),

			wantSuppressed:   0,
			wantRetained:     1,
			wantDelay:        m(4),
			wantLongTotal:    0,
			wantLongRetained: 0,
		},
		{
			// Candidate BELOW current for:: underlying is always
			// currentFor+d >= currentFor > candidateFor, so nothing is ever
			// suppressed, and Delay is 0 (nothing fires later; if anything
			// it would fire earlier, which Compute does not claim).
			name:          "candidate lower than current suppresses nothing",
			durations:     []time.Duration{m(3), m(10)},
			currentFor:    m(5),
			candidateFor:  m(2),
			longThreshold: m(10),

			wantSuppressed:   0,
			wantRetained:     2,
			wantDelay:        0,
			wantLongTotal:    1,
			wantLongRetained: 1,
		},
		{
			// Candidate EQUAL to current for:: no change in behaviour,
			// nothing suppressed, no delay.
			name:          "candidate equal to current suppresses nothing",
			durations:     []time.Duration{m(1), m(2)},
			currentFor:    m(1),
			candidateFor:  m(1),
			longThreshold: m(10),

			wantSuppressed:   0,
			wantRetained:     2,
			wantDelay:        0,
			wantLongTotal:    0,
			wantLongRetained: 0,
		},
		{
			// Mixed distribution, hand-computed:
			// durations 1m,2m,3m,4m,20m; currentFor=1m; candidateFor=5m.
			// underlying: 2m,3m,4m,5m,21m.
			// suppressed (underlying<5m): 2m,3m,4m -> 3 suppressed.
			// retained: 5m(boundary),21m -> 2 retained.
			// long (>=10m firing duration): only 20m -> 1 long, and it is
			// retained.
			name:          "mixed distribution",
			durations:     []time.Duration{m(1), m(2), m(3), m(4), m(20)},
			currentFor:    m(1),
			candidateFor:  m(5),
			longThreshold: m(10),

			wantSuppressed:   3,
			wantRetained:     2,
			wantDelay:        m(4),
			wantLongTotal:    1,
			wantLongRetained: 1,
		},
		{
			// Degenerate: no episodes at all.
			name:          "no episodes",
			durations:     nil,
			currentFor:    m(1),
			candidateFor:  m(5),
			longThreshold: m(10),

			wantSuppressed:   0,
			wantRetained:     0,
			wantDelay:        m(4),
			wantLongTotal:    0,
			wantLongRetained: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compute(tt.durations, tt.currentFor, tt.candidateFor, tt.longThreshold)

			if got.Total != len(tt.durations) {
				t.Errorf("Total = %d, want %d", got.Total, len(tt.durations))
			}
			if got.Suppressed != tt.wantSuppressed {
				t.Errorf("Suppressed = %d, want %d", got.Suppressed, tt.wantSuppressed)
			}
			if got.Retained != tt.wantRetained {
				t.Errorf("Retained = %d, want %d", got.Retained, tt.wantRetained)
			}
			if got.Delay != tt.wantDelay {
				t.Errorf("Delay = %v, want %v", got.Delay, tt.wantDelay)
			}
			if got.LongTotal != tt.wantLongTotal {
				t.Errorf("LongTotal = %d, want %d", got.LongTotal, tt.wantLongTotal)
			}
			if got.LongRetained != tt.wantLongRetained {
				t.Errorf("LongRetained = %d, want %d", got.LongRetained, tt.wantLongRetained)
			}
			if got.Suppressed+got.Retained != got.Total {
				t.Errorf("Suppressed(%d)+Retained(%d) != Total(%d)", got.Suppressed, got.Retained, got.Total)
			}
		})
	}
}

func TestCounterfactual_SuppressedRate(t *testing.T) {
	c := Compute([]time.Duration{m(1), m(2), m(3), m(4), m(20)}, m(1), m(5), m(10))
	// 3 of 5 suppressed, computed by hand above.
	if got, want := c.SuppressedRate(), 0.6; got != want {
		t.Errorf("SuppressedRate = %v, want %v", got, want)
	}

	empty := Compute(nil, m(1), m(5), m(10))
	if got := empty.SuppressedRate(); got != 0 {
		t.Errorf("SuppressedRate on empty distribution = %v, want 0", got)
	}
}

func TestSelectCandidateFor(t *testing.T) {
	tests := []struct {
		name       string
		durations  []time.Duration
		currentFor time.Duration
		want       time.Duration
	}{
		{
			name:       "no episodes returns current for unchanged",
			durations:  nil,
			currentFor: m(1),
			want:       m(1),
		},
		{
			name:       "single episode: P90 of one value is that value",
			durations:  []time.Duration{m(7)},
			currentFor: m(1),
			want:       m(1) + m(7),
		},
		{
			// Sorted durations 1..10 minutes, ceil(0.9*10)-1 = 8 (0-indexed)
			// -> the 9th smallest value, 9m. Candidate = current(1m) + 9m =
			// 10m. Hand-computed the same way internal/score.percentile
			// would rank it.
			name: "ten distinct durations",
			durations: []time.Duration{
				m(1), m(2), m(3), m(4), m(5), m(6), m(7), m(8), m(9), m(10),
			},
			currentFor: m(1),
			want:       m(10),
		},
		{
			// All episodes identical: P90 of a constant distribution is
			// that constant.
			name:       "identical durations",
			durations:  []time.Duration{m(4), m(4), m(4), m(4)},
			currentFor: m(1),
			want:       m(5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SelectCandidateFor(tt.durations, tt.currentFor); got != tt.want {
				t.Errorf("SelectCandidateFor = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectCandidateFor_neverBelowCurrent(t *testing.T) {
	durations := []time.Duration{m(1), m(2), m(3)}
	current := m(5)
	got := SelectCandidateFor(durations, current)
	if got < current {
		t.Errorf("SelectCandidateFor = %v, must never be below current for: %v", got, current)
	}
}

func TestSentence_shape(t *testing.T) {
	c := Compute([]time.Duration{m(1), m(2), m(3), m(4), m(20)}, m(1), m(5), m(10))
	got := Sentence(m(3), c)
	want := "p90 episode is 3m, current `for: 1m`; `for: 5m` would have suppressed 60% of past fires " +
		"and retained 1 of 1 episodes longer than 10m."
	if got != want {
		t.Errorf("Sentence =\n%q\nwant\n%q", got, want)
	}
}

func TestSentence_candidateNotAboveCurrent(t *testing.T) {
	c := Compute([]time.Duration{m(1), m(2)}, m(5), m(2), m(10))
	got := Sentence(m(2), c)
	want := "p90 episode is 2m, current `for: 5m`; `for: 2m` is not longer than the current value, " +
		"so it would have suppressed nothing -- all 2 episode(s) retained."
	if got != want {
		t.Errorf("Sentence =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{3 * time.Minute, "3m"},
		{time.Hour + 2*time.Minute, "1h2m"},
		{time.Hour, "1h"},
		{time.Hour + 5*time.Second, "1h0m5s"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.d); got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
