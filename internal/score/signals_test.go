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
		ep(1, "a", 0, time.Minute),              // short
		ep(1, "a", time.Hour, 2*time.Minute),    // short
		ep(1, "a", 2*time.Hour, 30*time.Minute), // long
		ep(1, "a", 3*time.Hour, 20*time.Minute), // long
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

func TestFlapRateGroupsByFingerprint(t *testing.T) {
	// Two fingerprints interleaved in time. Grouped per fingerprint, "a"'s
	// two episodes are nearly 6 hours apart (no flap) and "b" has only one
	// episode (no flap): the correct answer is 0. An implementation that
	// sorts globally without grouping by fingerprint would see a's first
	// episode end 5 minutes before b's episode starts and wrongly count that
	// as a re-fire.
	eps := []store.Episode{
		ep(1, "a", 0, 5*time.Minute),
		ep(1, "b", 10*time.Minute, 5*time.Minute),
		ep(1, "a", 6*time.Hour, 5*time.Minute),
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "flap_rate interleaved fingerprints", s.FlapRate, 0)
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

func TestSilencedRateAcrossMultipleEpisodesWithOverlappingSilences(t *testing.T) {
	// Two episodes of different lengths (60m and 20m), and two overlapping
	// silences covering the first episode (union [10m,50m] = 40m, not their
	// summed 20m+30m=50m) plus one silence covering 10m of the second
	// episode. Total silenced = 40m+10m = 50m over total firing 80m = 0.625.
	// This would come out differently if silences were summed instead of
	// unioned, or if the denominator were wall-clock span instead of the sum
	// of firing durations.
	eps := []store.Episode{
		ep(1, "a", 0, time.Hour),
		ep(1, "b", 120*time.Minute, 20*time.Minute),
	}
	matcher := []store.Matcher{{Name: "alertname", Value: "A", IsEqual: true}}
	sils := []store.Silence{
		{Matchers: matcher, StartsAt: base.Add(10 * time.Minute), EndsAt: base.Add(30 * time.Minute)},
		{Matchers: matcher, StartsAt: base.Add(20 * time.Minute), EndsAt: base.Add(50 * time.Minute)},
		{Matchers: matcher, StartsAt: base.Add(125 * time.Minute), EndsAt: base.Add(135 * time.Minute)},
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Silences: sils, Location: time.UTC})
	closeTo(t, "silenced_rate across episodes", s.SilencedRate, 0.625)
}

// TestNormaliseOffHoursRescalesAgainstTheWorkingWeek pins the rescaling that
// makes offhours_rate a signal at all.
//
// isOffHours calls 123 of the week's 168 hours off-hours (168 - 5 weekdays x 9
// business hours), so ANY rule firing round the clock scores 0.732 raw. The
// raw number therefore separated nothing, while adding a flat ~7.3 points to
// every noise score -- and noisyThreshold had been calibrated with that
// constant baked in. Normalised, the signal reports excess nocturnal firing.
func TestNormaliseOffHoursRescalesAgainstTheWorkingWeek(t *testing.T) {
	cases := []struct {
		name string
		raw  float64
		want float64
	}{
		{"never off-hours", 0, 0},
		{"mostly in-hours", 0.5, 0},
		{"exactly the uniform baseline", offHoursBaseline, 0},
		// Halfway between the baseline and always-nocturnal: (0.732+1)/2.
		{"halfway above the baseline", (offHoursBaseline + 1) / 2, 0.5},
		{"only ever at night", 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			closeTo(t, "normalised offhours_rate", normaliseOffHours(c.raw), c.want)
		})
	}
}

// TestOffhoursRateIsZeroForARuleFavouringOfficeHours is the same property
// measured end-to-end: half the fires in hours is BELOW the uniform baseline,
// so the rule contributes nothing to the noise score on this axis.
func TestOffhoursRateIsZeroForARuleFavouringOfficeHours(t *testing.T) {
	// base is Tuesday 10:00 UTC, inside hours. +12h is 22:00, outside.
	eps := []store.Episode{
		ep(1, "a", 0, time.Minute),
		ep(1, "a", 12*time.Hour, time.Minute),
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "offhours_rate", s.OffhoursRate, 0)
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

func TestOffhoursRateRespectsNonUTCLocation(t *testing.T) {
	// Tuesday 20:00 UTC is off-hours in UTC (>= 18:00), but the same instant
	// is 14:00 in a UTC-6 zone, which is in-hours. An implementation that
	// ignores Location entirely would give the same answer for both.
	instant := time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC)
	eps := []store.Episode{{
		RuleID: 1, Fingerprint: "a",
		StartedAt: instant, EndedAt: instant.Add(time.Minute),
		State: store.StateFiring,
	}}

	sUTC := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "offhours_rate in UTC", sUTC.OffhoursRate, 1.0)

	loc := time.FixedZone("UTC-6", -6*60*60)
	sLocal := Compute(Input{Rule: rule(0), Episodes: eps, Location: loc})
	closeTo(t, "offhours_rate in UTC-6", sLocal.OffhoursRate, 0.0)
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

func TestCofireRatioCountsDistinctRules(t *testing.T) {
	// Two other rules contribute five episodes between them: an
	// implementation that counts episodes rather than distinct rule IDs
	// would wrongly reach the threshold of three distinct others.
	own := []store.Episode{ep(1, "a", 0, time.Minute)}
	all := append([]store.Episode{}, own...)
	all = append(all,
		ep(2, "x1", 0, time.Minute),
		ep(2, "x2", 10*time.Second, time.Minute),
		ep(2, "x3", 20*time.Second, time.Minute),
		ep(3, "y1", 30*time.Second, time.Minute),
		ep(3, "y2", 40*time.Second, time.Minute),
	)
	s := Compute(Input{Rule: rule(0), Episodes: own, AllEpisodes: all, Location: time.UTC})
	closeTo(t, "cofire_ratio with 2 distinct rules across 5 episodes", s.CofireRatio, 0)
}

func TestCofireRatioIgnoresPendingEpisodes(t *testing.T) {
	// Nobody was paged for a pending alert, so three other rules co-occurring
	// only as pending episodes must not count as co-firing.
	own := []store.Episode{ep(1, "a", 0, time.Minute)}
	pendingEp := func(ruleID int64, fp string, offset time.Duration) store.Episode {
		e := ep(ruleID, fp, offset, time.Minute)
		e.State = store.StatePending
		return e
	}
	all := append([]store.Episode{}, own...)
	all = append(all,
		pendingEp(2, "b", 30*time.Second),
		pendingEp(3, "c", 30*time.Second),
		pendingEp(4, "d", 30*time.Second),
	)
	s := Compute(Input{Rule: rule(0), Episodes: own, AllEpisodes: all, Location: time.UTC})
	closeTo(t, "cofire_ratio ignores pending others", s.CofireRatio, 0)
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
	// Two fingerprints, maximally skewed: this is the regression guard for
	// the small-sample correction. Without it, Gini over two positive counts
	// tops out at 0.50 and could never clear a 0.60 verdict threshold.
	var eps []store.Episode
	for i := 0; i < 20; i++ {
		eps = append(eps, ep(1, "hot", time.Duration(i)*time.Hour, time.Minute))
	}
	eps = append(eps, ep(1, "cold", 100*time.Hour, time.Minute))

	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	if s.Concentration < 0.6 {
		t.Errorf("concentration = %v, want > 0.6 for a heavily skewed two-fingerprint rule", s.Concentration)
	}
}

func TestConcentrationPinnedValue(t *testing.T) {
	// Four fingerprints with counts 1,1,1,7 (10 episodes). Uncorrected Gini
	// here is exactly 0.45; the n/(n-1) small-sample correction (n=4) scales
	// it to exactly 0.6.
	var eps []store.Episode
	eps = append(eps, ep(1, "a", 0, time.Minute))
	eps = append(eps, ep(1, "b", time.Hour, time.Minute))
	eps = append(eps, ep(1, "c", 2*time.Hour, time.Minute))
	for i := 0; i < 7; i++ {
		eps = append(eps, ep(1, "d", time.Duration(3+i)*time.Hour, time.Minute))
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	closeTo(t, "concentration", s.Concentration, 0.6)
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
	// A rule with a 10m for: clause -- large enough that an implementation
	// using rule.For as the match tolerance (rather than one sample step)
	// would wrongly match episodes far away or on the wrong side of the
	// pending period.
	pendingAt := func(offset time.Duration, fp string) store.Episode {
		e := ep(1, fp, offset, 2*time.Minute) // Resolution: time.Minute via ep()
		e.State = store.StatePending
		return e
	}

	// p1 ends at 2m; a firing episode starts exactly one Resolution (1m)
	// later, at 3m: this is the very next evaluation, so it became firing.
	p1 := pendingAt(0, "p1")
	f1 := ep(1, "p1", 3*time.Minute, 5*time.Minute)

	// p2 ends at 102m; a firing episode starts 10m later. That is far beyond
	// one sample step, so this is churn -- not the pending period becoming
	// real -- even though it would fall inside a for:-sized (10m) tolerance.
	p2 := pendingAt(100*time.Minute, "p2")
	f2 := ep(1, "p2", 112*time.Minute, 5*time.Minute)

	// p3 ends at 302m; a firing episode on the same fingerprint STARTED 5m
	// BEFORE p3 ended. That firing episode is a different, already-open
	// episode overlapping the pending period, not this pending period
	// becoming firing -- so this is churn, not "became".
	p3 := pendingAt(300*time.Minute, "p3")
	f3 := ep(1, "p3", 297*time.Minute, 5*time.Minute)

	eps := []store.Episode{p1, f1, p2, f2, p3, f3}
	s := Compute(Input{Rule: rule(10 * time.Minute), Episodes: eps, Location: time.UTC})
	closeTo(t, "pending_churn", s.PendingChurn, 2.0/3.0)
}

func TestUnknownStateExcludedFromFires(t *testing.T) {
	e := ep(1, "a", 0, time.Minute)
	e.State = ""
	s := Compute(Input{Rule: rule(0), Episodes: []store.Episode{e}, Location: time.UTC})
	if s.Fires != 0 {
		t.Errorf("fires = %d, want 0: an unknown state is not evidence of firing", s.Fires)
	}
}

func TestShortLivedThresholdIsClamped(t *testing.T) {
	cases := []struct {
		name   string
		forDur time.Duration
		want   time.Duration
	}{
		{"zero", 0, 5 * time.Minute},
		{"below floor after scaling", time.Minute, 5 * time.Minute},
		{"scales within range", 10 * time.Minute, 30 * time.Minute},
		{"clamped by cap", 2 * time.Hour, 30 * time.Minute},
		{"absurd for near overflow bound", 291 * 365 * 24 * time.Hour, 30 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shortLivedThreshold(rule(c.forDur))
			if got != c.want {
				t.Errorf("shortLivedThreshold(for=%v) = %v, want %v", c.forDur, got, c.want)
			}
		})
	}
}

func TestSignalsMapContainsDurationSignals(t *testing.T) {
	// Two episodes, 2m and 4m: p50 picks the smaller (index 0), p90 picks the
	// larger (index 1). Tasks 11 and 12 bind to these exact keys.
	eps := []store.Episode{
		ep(1, "a", 0, 2*time.Minute),
		ep(1, "a", time.Hour, 4*time.Minute),
	}
	s := Compute(Input{Rule: rule(0), Episodes: eps, Location: time.UTC})
	m := s.Map()
	closeTo(t, "p50_duration_s", m["p50_duration_s"], 120)
	closeTo(t, "p90_duration_s", m["p90_duration_s"], 240)
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
