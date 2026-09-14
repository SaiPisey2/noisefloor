package score

import (
	"math"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

var base = time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC) // a Tuesday, 10:00

func ep(ruleID int64, fp string, offset, dur time.Duration) store.Episode {
	return store.Episode{
		RuleID: ruleID, Fingerprint: fp,
		StartedAt: base.Add(offset), EndedAt: base.Add(offset + dur),
		Resolution: time.Minute, Source: store.SourceBackfill,
		State: store.StateFiring,
	}
}

func rule(forDur time.Duration) store.Rule {
	return store.Rule{ID: 1, AlertName: "A", GroupName: "g", For: forDur}
}

func closeTo(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.001 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestComputeEmptyInput(t *testing.T) {
	s := Compute(Input{Rule: rule(0), Location: time.UTC})
	if s.Fires != 0 || s.ShortLivedRate != 0 || s.Concentration != 0 {
		t.Errorf("empty input produced non-zero signals: %+v", s)
	}
}

func TestShortLivedRateUsesFloorOfFiveMinutes(t *testing.T) {
	// for: 0 means the threshold is the 5m floor.
	eps := []store.Episode{
		ep(1, "a", 0, time.Minute),               // short
		ep(1, "a", time.Hour, 2*time.Minute),      // short
		ep(1, "a", 2*time.Hour, 30*time.Minute),   // long
		ep(1, "a", 3*time.Hour, 20*time.Minute),   // long
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "short_lived_rate", s.ShortLivedRate, 0.5)
}

func TestShortLivedRateScalesWithFor(t *testing.T) {
	// for: 10m means the threshold is 30m, so a 20m episode is short.
	eps := []store.Episode{
		ep(1, "a", 0, 20*time.Minute),
		ep(1, "a", 2*time.Hour, 45*time.Minute),
	}
	s := Compute(Input{Rule: rule(10 * time.Minute), Episodes: eps, Location: time.UTC})
	closeTo(t, "short_lived_rate", s.ShortLivedRate, 0.5)
}

func TestFlapRateCountsQuickRefires(t *testing.T) {
	// Three episodes on one fingerprint, each restarting 10m after the last
	// ended: two of the three are re-fires.
	eps := []store.Episode{
		ep(1, "a", 0, 5*time.Minute),
		ep(1, "a", 15*time.Minute, 5*time.Minute),
		ep(1, "a", 30*time.Minute, 5*time.Minute),
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "flap_rate", s.FlapRate, 2.0/3.0)
}

func TestFlapRateIgnoresDistantRefires(t *testing.T) {
	eps := []store.Episode{
		ep(1, "a", 0, 5*time.Minute),
		ep(1, "a", 6*time.Hour, 5*time.Minute),
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "flap_rate", s.FlapRate, 0)
}

func TestSilencedRateIsFractionOfFiringTime(t *testing.T) {
	eps := []store.Episode{ep(1, "a", 0, time.Hour)}
	sils := []store.Silence{{
		Matchers: []store.Matcher{{Name: "alertname", Value: "A", IsEqual: true}},
		StartsAt: base, EndsAt: base.Add(30 * time.Minute),
	}}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Silences: sils, Location: time.UTC})
	closeTo(t, "silenced_rate", s.SilencedRate, 0.5)
}

func TestOffhoursRateUsesConfiguredLocation(t *testing.T) {
	// base is Tuesday 10:00 UTC, inside hours. +12h is 22:00, outside.
	eps := []store.Episode{
		ep(1, "a", 0, time.Minute),
		ep(1, "a", 12*time.Hour, time.Minute),
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "offhours_rate", s.OffhoursRate, 0.5)
}

func TestOffhoursRateTreatsWeekendsAsOffHours(t *testing.T) {
	saturday := time.Date(2026, 3, 14, 11, 0, 0, 0, time.UTC)
	eps := []store.Episode{{
		RuleID: 1, Fingerprint: "a",
		StartedAt: saturday, EndedAt: saturday.Add(time.Minute),
		State: store.StateFiring,
	}}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "offhours_rate", s.OffhoursRate, 1.0)
}

func TestCofireRatioNeedsThreeOtherRules(t *testing.T) {
	own := []store.Episode{ep(1, "a", 0, time.Minute)}

	// Only two other rules fire alongside: not enough.
	twoOthers := append([]store.Episode{}, own...)
	twoOthers = append(twoOthers, ep(2, "b", 30*time.Second, time.Minute))
	twoOthers = append(twoOthers, ep(3, "c", 30*time.Second, time.Minute))

	s := Compute(Input{Rule: rule(0), Episodes: own, AllEpisodes: twoOthers, Location: time.UTC})
	closeTo(t, "cofire_ratio with 2 others", s.CofireRatio, 0)

	threeOthers := append([]store.Episode{}, twoOthers...)
	threeOthers = append(threeOthers, ep(4, "d", 30*time.Second, time.Minute))

	s = Compute(Input{Rule: rule(0), Episodes: own, AllEpisodes: threeOthers, Location: time.UTC})
	closeTo(t, "cofire_ratio with 3 others", s.CofireRatio, 1.0)
}

func TestCofireRatioIgnoresDistantFires(t *testing.T) {
	own := []store.Episode{ep(1, "a", 0, time.Minute)}
	all := append([]store.Episode{}, own...)
	for i, id := range []int64{2, 3, 4} {
		all = append(all, ep(id, "x", time.Duration(i+10)*time.Minute, time.Minute))
	}
	s := Compute(Input{Rule: rule(0), Episodes: own, AllEpisodes: all, Location: time.UTC})
	closeTo(t, "cofire_ratio", s.CofireRatio, 0)
}

func TestConcentrationIsZeroWhenEven(t *testing.T) {
	eps := []store.Episode{
		ep(1, "a", 0, time.Minute),
		ep(1, "b", time.Hour, time.Minute),
		ep(1, "c", 2*time.Hour, time.Minute),
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "concentration", s.Concentration, 0)
	if s.UniqueFingerprints != 3 {
		t.Errorf("unique fingerprints = %d, want 3", s.UniqueFingerprints)
	}
}

func TestConcentrationIsHighWhenSkewed(t *testing.T) {
	var eps []store.Episode
	for i := 0; i < 20; i++ {
		eps = append(eps, ep(1, "hot", time.Duration(i)*time.Hour, time.Minute))
	}
	eps = append(eps, ep(1, "cold", 100*time.Hour, time.Minute))

	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	if s.Concentration < 0.4 {
		t.Errorf("concentration = %v, want > 0.4 for a heavily skewed rule", s.Concentration)
	}
}

func TestPercentileDurations(t *testing.T) {
	var eps []store.Episode
	for i := 1; i <= 10; i++ {
		eps = append(eps, ep(1, "a", time.Duration(i)*time.Hour, time.Duration(i)*time.Minute))
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	if s.P50Duration != 5*time.Minute {
		t.Errorf("p50 = %v, want 5m", s.P50Duration)
	}
	if s.P90Duration != 9*time.Minute {
		t.Errorf("p90 = %v, want 9m", s.P90Duration)
	}
}

func TestPendingChurn(t *testing.T) {
	pending := func(offset time.Duration) store.Episode {
		e := ep(1, "a", offset, 2*time.Minute)
		e.State = store.StatePending
		return e
	}
	eps := []store.Episode{
		pending(0),                    // never reaches firing
		pending(time.Hour),            // followed by a firing episode
		ep(1, "a", time.Hour+2*time.Minute, 10*time.Minute),
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "pending_churn", s.PendingChurn, 0.5)
}

func TestSignalsMapContainsScoredSignals(t *testing.T) {
	s := Compute(Input{Rule: rule(0), Episodes: []store.Episode{ep(1, "a", 0, time.Minute)}, Location: time.UTC})
	m := s.Map()
	for _, key := range []string{
		"short_lived_rate", "silenced_rate", "flap_rate",
		"cofire_ratio", "offhours_rate", "concentration", "pending_churn",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("Map() missing key %s", key)
		}
	}
}
