package pr

import (
	"fmt"
	"strings"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// Evidence is what every proposal body states in numbers a reviewer who
// does not trust noisefloor can check against their own Prometheus: the
// observation window, how many episodes were counted, and what fraction of
// them were short-lived or silenced.
type Evidence struct {
	Group, AlertName string

	Fires          int
	ShortLivedRate float64
	SilencedRate   float64
	Confidence     float64

	// ShortLivedThreshold is the duration an episode has to beat to not
	// count towards ShortLivedRate, for this rule specifically. It is
	// carried rather than described because "short-lived" is not a constant:
	// score.ShortLivedThreshold scales it with the rule's own `for:`. A
	// body that says "% of them were short-lived" without saying what
	// short-lived meant here cannot be reproduced by the reviewer it is
	// addressed to.
	ShortLivedThreshold time.Duration
	// RuleFor is the rule's current `for:`, the one input
	// ShortLivedThreshold is derived from. Stating it alongside the
	// threshold is what makes the threshold checkable rather than merely
	// quoted: a reviewer can redo clamp(3 x for, 5m, 30m) themselves.
	RuleFor time.Duration

	WindowStart, WindowEnd time.Time

	// SilencedBy is every silence that covered at least one of this rule's
	// firing episodes in the window -- collect.CoveringSilences. Who
	// silenced it, and when.
	SilencedBy []store.Silence

	// SilencesAvailable is false when Alertmanager could not be reached (or
	// was never configured) for the scan this evidence came from. SilencedRate
	// then reads 0 and SilencedBy is empty -- not because nothing silenced
	// the rule, but because nobody looked.
	//
	// Carried explicitly because the two cases are indistinguishable in the
	// numbers and opposite in meaning. "0% silenced, nobody silenced this"
	// is the single strongest argument a retire PR makes to a stranger, and
	// asserting it from a failed fetch is presenting an unmeasured number as
	// a measured one.
	SilencesAvailable bool

	// PagerOutcomes is real pager evidence for this rule (issue #14), when
	// a pager enricher matched at least one of its episodes. Nil for
	// every proposal built without an enricher configured, which is every
	// proposal before this feature -- the body then reads exactly as it
	// always did. See RetireBody and TuneBody's evidenceStatement, which
	// this drives: a proposal backed by "N pages, X% acknowledged" is a
	// categorically stronger argument than one backed by duration alone,
	// and the body says which it has.
	PagerOutcomes *score.PagerOutcomes
}

// evidenceStatement is the one sentence every proposal body opens its
// evidence section with, stating plainly whether the numbers below are
// MEASURED (real pager outcomes) or ESTIMATED (inferred from firing
// duration and silence history) -- the same distinction the scan table,
// the API and the web UI surface, worded for a reviewer who does not use
// any of those and only ever sees this PR.
func evidenceStatement(p *score.PagerOutcomes) string {
	if p == nil {
		return "This proposal is backed by ESTIMATED signals only: episode duration and " +
			"silence history, inferred from firing shape. No pager enricher was configured " +
			"for this scan, so no real acknowledgement data was available."
	}
	return fmt.Sprintf(
		"This proposal is backed in part by MEASURED pager outcomes via %s: %d pages "+
			"covering %s of this rule's episodes, %s of them acknowledged and %s escalated -- "+
			"real outcomes, not inferred from episode duration. See how that evidence is allowed "+
			"to affect scoring in internal/score/verdict.go.",
		p.Source, p.Matched, formatPercent(p.Coverage), formatPercent(p.AckRate), formatPercent(p.EscalationRate))
}

func (e Evidence) windowDays() float64 {
	return e.WindowEnd.Sub(e.WindowStart).Hours() / 24
}

// RetireBody renders a retire proposal's PR body: the evidence table the
// design doc and issue #8 specify (fires, short-lived %, silenced % with
// who and when, confidence, observation window), plus a "how to check
// this" section a reviewer can run themselves.
//
// When e.SilencesAvailable is false the silence row and the "silenced by"
// section say so outright instead of printing 0% and "nobody". The
// proposal is still made, and that is a deliberate choice rather than an
// oversight:
//
//   - A missing silenced_rate can only read 0, which can only LOWER a
//     rule's noise score (silenced_rate carries 0.25 of it). A retire
//     verdict reached without silence evidence therefore cleared the
//     threshold on the other signals alone -- it is strictly more
//     conservative than one reached with it, never inflated by the gap.
//   - Refusing instead would make whether a rule gets a proposal depend on
//     Alertmanager's uptime during the scan, which is not a property of the
//     rule. A transient outage would drop legitimate proposals silently,
//     leaving no record anywhere for anyone to notice.
//   - The defect being fixed is the false CLAIM, not the proposal. "0%
//     silenced" and "no silence covered this rule" are assertions about a
//     measurement that was never taken; saying so removes the dishonesty
//     completely and hands the reviewer the caveat instead of hiding it.
func RetireBody(e Evidence) string {
	var b strings.Builder

	// The rule's own name and group are remote-sourced: whoever writes the
	// alerting rules chooses them, and this body is read by whoever reviews
	// the pull request. They are escaped with mdText and NOT wrapped in
	// backticks -- a code span can be closed by a backtick in the value,
	// and a backslash escape does not work inside one, so plain escaped
	// text is the shape that cannot be broken out of.
	fmt.Fprintf(&b, "## noisefloor: retire %s\n\n", mdText(e.AlertName))
	fmt.Fprintf(&b,
		"Proposed for deletion: %s / %s. This rule crosses noisefloor's retire "+
			"threshold on the evidence below, gathered over the observation window "+
			"stated here -- not asserted, checkable.\n\n", mdText(e.Group), mdText(e.AlertName))

	fmt.Fprintf(&b, "%s\n\n", evidenceStatement(e.PagerOutcomes))

	b.WriteString("| metric | value |\n")
	b.WriteString("| --- | --- |\n")
	fmt.Fprintf(&b, "| fires | %d |\n", e.Fires)
	fmt.Fprintf(&b, "| short-lived | %s (shorter than %s, i.e. resolved before anyone could act) |\n",
		formatPercent(e.ShortLivedRate), formatDuration(e.ShortLivedThreshold))
	if e.SilencesAvailable {
		fmt.Fprintf(&b, "| silenced | %s |\n", formatPercent(e.SilencedRate))
	} else {
		b.WriteString("| silenced | **not measured** -- Alertmanager was unavailable for this scan |\n")
	}
	if e.PagerOutcomes != nil {
		fmt.Fprintf(&b, "| pager acknowledged (measured, %s coverage) | %s |\n",
			formatPercent(e.PagerOutcomes.Coverage), formatPercent(e.PagerOutcomes.AckRate))
		fmt.Fprintf(&b, "| pager escalated (measured) | %s |\n", formatPercent(e.PagerOutcomes.EscalationRate))
		// Reported alongside ack/escalation because it is the fact that can
		// refute a retire proposal reached via the never-acked upgrade (see
		// internal/score/verdict.go's applyMeasuredOutcomes): a team that
		// resolves from the push notification without formally acknowledging
		// the page reads as "never engaged" on the two rows above alone.
		fmt.Fprintf(&b, "| pager human-resolved (measured) | %s |\n", formatPercent(e.PagerOutcomes.HumanResolvedRate))
	}
	fmt.Fprintf(&b, "| confidence | %.2f |\n", e.Confidence)
	fmt.Fprintf(&b, "| observation window | %s to %s (%s) |\n",
		e.WindowStart.Format(time.RFC3339), e.WindowEnd.Format(time.RFC3339), formatWindow(e.windowDays()))
	b.WriteString("\n")

	b.WriteString(silencedBySection(e.SilencedBy, e.SilencesAvailable))
	b.WriteString("\n")

	fmt.Fprintf(&b,
		"**How to check this**\n\nRun the same range query against your own "+
			"Prometheus, over the same window:\n\n    ALERTS{alertname=%q}\n\n"+
			"Count episodes as one continuous run of `alertstate=\"firing\"` samples "+
			"on a single series, gap-tolerant to one query step. You should find "+
			"approximately %d episodes, %s of them shorter than %s.\n\n"+
			"That %s figure is this rule's short-lived threshold: three times its "+
			"`for: %s`, clamped to the range 5m..30m.\n\n",
		e.AlertName, e.Fires, formatPercent(e.ShortLivedRate),
		formatDuration(e.ShortLivedThreshold), formatDuration(e.ShortLivedThreshold),
		formatDuration(e.RuleFor))

	b.WriteString("---\n")
	if !e.SilencesAvailable {
		b.WriteString("Generated by noisefloor from stored ALERTS history over the window above. " +
			"Silence history was NOT available for this scan, so no claim is made about it " +
			"either way -- weigh this proposal on the remaining evidence only.\n")
		return b.String()
	}
	b.WriteString("Generated by noisefloor from stored ALERTS and Alertmanager " +
		"silence history over the window above.\n")
	return b.String()
}

// TuneBody renders a tune proposal's PR body: the counterfactual sentence
// stated explicitly (remediate.Sentence, passed in already computed --
// this package does not re-derive it), plus the same checkable window and
// episode count a retire body carries.
func TuneBody(e Evidence, currentFor, candidateFor time.Duration, sentence string) string {
	var b strings.Builder

	// See RetireBody: remote-sourced, escaped, and not in a code span.
	fmt.Fprintf(&b, "## noisefloor: tune %s\n\n", mdText(e.AlertName))
	fmt.Fprintf(&b, "Proposed change: %s / %s. Raise `for: %s` to `for: %s`.\n\n",
		mdText(e.Group), mdText(e.AlertName), formatDuration(currentFor), formatDuration(candidateFor))
	fmt.Fprintf(&b, "%s\n\n", sentence)
	fmt.Fprintf(&b, "%s\n\n", evidenceStatement(e.PagerOutcomes))

	b.WriteString("| metric | value |\n")
	b.WriteString("| --- | --- |\n")
	fmt.Fprintf(&b, "| fires | %d |\n", e.Fires)
	if e.PagerOutcomes != nil {
		fmt.Fprintf(&b, "| pager acknowledged (measured, %s coverage) | %s |\n",
			formatPercent(e.PagerOutcomes.Coverage), formatPercent(e.PagerOutcomes.AckRate))
	}
	fmt.Fprintf(&b, "| confidence | %.2f |\n", e.Confidence)
	fmt.Fprintf(&b, "| observation window | %s to %s (%s) |\n",
		e.WindowStart.Format(time.RFC3339), e.WindowEnd.Format(time.RFC3339), formatWindow(e.windowDays()))
	b.WriteString("\n")

	fmt.Fprintf(&b,
		"**How to check this**\n\nRun the same range query against your own "+
			"Prometheus, over the same window:\n\n    ALERTS{alertname=%q}\n\n"+
			"For each episode, add the CURRENT `for: %s` back onto its firing "+
			"duration -- that is how long the underlying condition actually held, "+
			"which is what a candidate `for:` is compared against (see the "+
			"counterfactual sentence above). Count how many of the %d episodes "+
			"stay under the candidate `for: %s`; that share is the suppressed rate "+
			"stated above.\n\n",
		e.AlertName, formatDuration(currentFor), e.Fires, formatDuration(candidateFor))

	b.WriteString("---\nGenerated by noisefloor from stored ALERTS history over the window above.\n")
	return b.String()
}

func silencedBySection(sils []store.Silence, available bool) string {
	if !available {
		return "**Silenced by:** unknown -- Alertmanager could not be reached during this scan, " +
			"so silence history was never fetched. This is NOT a finding that nobody silenced " +
			"the rule; it is the absence of a finding. If your team has been silencing this " +
			"alert, that evidence is missing from the table above and from the score behind " +
			"this proposal.\n"
	}
	if len(sils) == 0 {
		return "**Silenced by:** nobody -- no silence covered this rule's firing time in the window.\n"
	}
	var b strings.Builder
	b.WriteString("**Silenced by:**\n\n")
	// A silence's author and comment are the least trusted strings in this
	// whole body: the comment is free text, written by whoever created the
	// silence through Alertmanager, and it lands in a list item in someone
	// else's repository.
	for _, s := range sils {
		comment := s.Comment
		if comment == "" {
			comment = "(no comment)"
		}
		fmt.Fprintf(&b, "- %s, %s to %s: %s\n",
			mdText(s.CreatedBy), s.StartsAt.Format(time.RFC3339), s.EndsAt.Format(time.RFC3339), mdText(comment))
	}
	return b.String()
}

func formatPercent(rate float64) string {
	return fmt.Sprintf("%.0f%%", rate*100)
}

// formatWindow renders an observation window's length in whole days when it
// divides evenly, and fractional otherwise -- "30d" reads better than
// "30.0d", but a truncated window (e.g. a fresh Prometheus) still needs the
// fraction to be honest about its length.
func formatWindow(days float64) string {
	if days == float64(int(days)) {
		return fmt.Sprintf("%.0fd", days)
	}
	return fmt.Sprintf("%.1fd", days)
}

// formatDuration renders a duration compactly, the same shape
// internal/remediate and internal/report use: every unit
// time.Duration.String would print gets noisy at episode-duration
// precision (3m0s, 1h2m0s), and this goes straight into prose and a `for:`
// value a reviewer reads once.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)

	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	units := [3]struct {
		v int64
		u string
	}{{int64(h), "h"}, {int64(m), "m"}, {int64(s), "s"}}

	first := -1
	for i, u := range units {
		if u.v != 0 {
			first = i
			break
		}
	}
	if first == -1 {
		return "0s"
	}
	last := len(units) - 1
	for last > first && units[last].v == 0 {
		last--
	}

	out := ""
	for i := first; i <= last; i++ {
		out += fmt.Sprintf("%d%s", units[i].v, units[i].u)
	}
	return out
}
