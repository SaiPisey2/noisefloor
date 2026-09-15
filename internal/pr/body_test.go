package pr

import (
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/score"
)

func measuredEvidence() Evidence {
	return Evidence{
		Group: "demo", AlertName: "DemoSpiky",
		Fires: 120, ShortLivedRate: 0.9, SilencedRate: 0.4, Confidence: 0.85,
		ShortLivedThreshold: 9 * time.Minute, RuleFor: 3 * time.Minute,
		WindowStart:       fixedWindowStart,
		WindowEnd:         fixedWindowEnd,
		SilencesAvailable: true,
	}
}

// TestRetireBodyStatesTheShortLivedThreshold is finding 6: a reviewer
// cannot derive "90% of them were short-lived" from anything in the body
// unless the body says what short-lived MEANT for this rule -- the
// threshold is clamp(3 x for, 5m, 30m), not a constant.
func TestRetireBodyStatesTheShortLivedThreshold(t *testing.T) {
	body := RetireBody(measuredEvidence())

	if strings.Contains(body, "this rule's short-lived threshold.") {
		t.Error("the body still refers to the threshold without stating it")
	}
	// The computed value, the input it came from, and the clamp -- enough to
	// redo the arithmetic.
	for _, want := range []string{"9m", "for: 3m", "5m..30m"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not state %q:\n%s", want, body)
		}
	}
}

// TestRetireBodyThresholdFloorIsStated covers the clamp's lower bound: a
// rule with no `for:` has 3 x 0s = 0s scaled up to the 5m floor, and the
// body has to show the floor rather than "0s".
func TestRetireBodyThresholdFloorIsStated(t *testing.T) {
	e := measuredEvidence()
	e.RuleFor = 0
	e.ShortLivedThreshold = 5 * time.Minute

	body := RetireBody(e)
	if !strings.Contains(body, "shorter than 5m") {
		t.Errorf("body does not state the clamped 5m floor:\n%s", body)
	}
	if !strings.Contains(body, "for: 0s") {
		t.Errorf("body does not state the for: the threshold was derived from:\n%s", body)
	}
}

// TestRetireBodyDoesNotAssertUnmeasuredSilences is finding 5. The scanner
// already knows Alertmanager was unreachable; without that flag reaching
// the body, a retire PR tells a stranger "0% silenced" and "no silence
// covered this rule's firing time" -- two assertions about a measurement
// that was never taken, and the strongest claims the PR makes.
func TestRetireBodyDoesNotAssertUnmeasuredSilences(t *testing.T) {
	e := measuredEvidence()
	e.SilencesAvailable = false
	e.SilencedRate = 0
	e.SilencedBy = nil

	body := RetireBody(e)

	if strings.Contains(body, "| silenced | 0% |") {
		t.Error("body asserts a silenced rate of 0% that was never measured")
	}
	if strings.Contains(body, "nobody -- no silence covered") {
		t.Error("body asserts nobody silenced the rule, which was never checked")
	}
	if !strings.Contains(body, "not measured") {
		t.Errorf("the silenced row does not say the data is missing:\n%s", body)
	}
	if !strings.Contains(body, "Alertmanager could not be reached") {
		t.Errorf("the silenced-by section does not say the data is missing:\n%s", body)
	}
	// And the footer must not go on citing a source it never read.
	if strings.Contains(body, "and Alertmanager silence history over the window") {
		t.Error("the footer still credits Alertmanager silence history it never fetched")
	}
}

// TestRetireBodyStatesMeasuredSilencesNormally is the other direction: when
// the data IS there, nothing about it is hedged.
func TestRetireBodyStatesMeasuredSilencesNormally(t *testing.T) {
	body := RetireBody(measuredEvidence())

	if !strings.Contains(body, "| silenced | 40% |") {
		t.Errorf("body does not state the measured silenced rate:\n%s", body)
	}
	if strings.Contains(body, "not measured") || strings.Contains(body, "could not be reached") {
		t.Errorf("body hedges a silenced rate that was actually measured:\n%s", body)
	}
}

// TestRetireBodyStatesHumanResolvedRate is finding 2's second half: the
// keep-to-retire upgrade can be refuted by HumanResolvedRate, so a
// reviewer of a proposal that upgrade produced must be able to see that
// number. The body reported coverage, ack and escalation only.
func TestRetireBodyStatesHumanResolvedRate(t *testing.T) {
	e := measuredEvidence()
	e.PagerOutcomes = &score.PagerOutcomes{
		Source: "pagerduty", Coverage: 1, Matched: 80,
		AckRate: 0, EscalationRate: 0, HumanResolvedRate: 1,
	}

	body := RetireBody(e)
	if !strings.Contains(body, "human resolved") && !strings.Contains(body, "human-resolved") {
		t.Errorf("body does not report HumanResolvedRate at all:\n%s", body)
	}
	if !strings.Contains(body, "100%") {
		t.Errorf("body does not state the measured 100%% human-resolved rate:\n%s", body)
	}
}
